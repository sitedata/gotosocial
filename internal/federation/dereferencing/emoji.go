// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package dereferencing

import (
	"context"
	"errors"
	"io"
	"net/url"

	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/media"
	"github.com/superseriousbusiness/gotosocial/internal/util"
)

// GetEmoji fetches the emoji with given shortcode,
// domain and remote URL to dereference it by. This
// handles the case of existing emojis by passing them
// to RefreshEmoji(), which in the case of a local
// emoji will be a no-op. If the emoji does not yet
// exist it will be newly inserted into the database
// followed by dereferencing the actual media file.
//
// Please note that even if an error is returned,
// an emoji model may still be returned if the error
// was only encountered during actual dereferencing.
// In this case, it will act as a placeholder.
func (d *Dereferencer) GetEmoji(
	ctx context.Context,
	shortcode string,
	domain string,
	remoteURL string,
	info media.AdditionalEmojiInfo,
	refresh bool,
) (
	*gtsmodel.Emoji,
	error,
) {
	// Look for an existing emoji with shortcode domain.
	emoji, err := d.state.DB.GetEmojiByShortcodeDomain(ctx,
		shortcode,
		domain,
	)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		return nil, gtserror.Newf("error fetching emoji from db: %w", err)
	}

	if emoji != nil {
		// This was an existing emoji, pass to refresh func.
		return d.RefreshEmoji(ctx, emoji, info, refresh)
	}

	if domain == "" {
		// failed local lookup, will be db.ErrNoEntries.
		return nil, gtserror.SetUnretrievable(err)
	}

	// Generate shortcode domain for locks + logging.
	shortcodeDomain := shortcode + "@" + domain

	// Ensure we have a valid remote URL.
	url, err := url.Parse(remoteURL)
	if err != nil {
		err := gtserror.Newf("invalid image remote url %s for emoji %s: %w", remoteURL, shortcodeDomain, err)
		return nil, err
	}

	// Acquire new instance account transport for emoji dereferencing.
	tsport, err := d.transportController.NewTransportForUsername(ctx, "")
	if err != nil {
		err := gtserror.Newf("error getting instance transport: %w", err)
		return nil, err
	}

	// Prepare data function to dereference remote emoji media.
	data := func(context.Context) (io.ReadCloser, int64, error) {
		return tsport.DereferenceMedia(ctx, url)
	}

	// Pass along for safe processing.
	return d.processEmojiSafely(ctx,
		shortcodeDomain,
		func() (*media.ProcessingEmoji, error) {
			return d.mediaManager.CreateEmoji(ctx,
				shortcode,
				domain,
				data,
				info,
			)
		},
	)
}

// RefreshEmoji ensures that the given emoji is
// up-to-date, both in terms of being cached in
// in local instance storage, and compared to extra
// information provided in media.AdditionEmojiInfo{}.
// (note that is a no-op to pass in a local emoji).
//
// Please note that even if an error is returned,
// an emoji model may still be returned if the error
// was only encountered during actual dereferencing.
// In this case, it will act as a placeholder.
func (d *Dereferencer) RefreshEmoji(
	ctx context.Context,
	emoji *gtsmodel.Emoji,
	info media.AdditionalEmojiInfo,
	force bool,
) (
	*gtsmodel.Emoji,
	error,
) {
	// Can't refresh local.
	if emoji.IsLocal() {
		return emoji, nil
	}

	// Check emoji is up-to-date
	// with provided extra info.
	switch {
	case info.URI != nil &&
		*info.URI != emoji.URI:
		force = true
	case info.ImageRemoteURL != nil &&
		*info.ImageRemoteURL != emoji.ImageRemoteURL:
		force = true
	case info.ImageStaticRemoteURL != nil &&
		*info.ImageStaticRemoteURL != emoji.ImageStaticRemoteURL:
		force = true
	}

	// Check if needs updating.
	if !force && *emoji.Cached {
		return emoji, nil
	}

	// TODO: more finegrained freshness checks.

	// Generate shortcode domain for locks + logging.
	shortcodeDomain := emoji.Shortcode + "@" + emoji.Domain

	// Ensure we have a valid image remote URL.
	url, err := url.Parse(emoji.ImageRemoteURL)
	if err != nil {
		err := gtserror.Newf("invalid image remote url %s for emoji %s: %w", emoji.ImageRemoteURL, shortcodeDomain, err)
		return nil, err
	}

	// Acquire new instance account transport for emoji dereferencing.
	tsport, err := d.transportController.NewTransportForUsername(ctx, "")
	if err != nil {
		err := gtserror.Newf("error getting instance transport: %w", err)
		return nil, err
	}

	// Prepare data function to dereference remote emoji media.
	data := func(context.Context) (io.ReadCloser, int64, error) {
		return tsport.DereferenceMedia(ctx, url)
	}

	// Pass along for safe processing.
	return d.processEmojiSafely(ctx,
		shortcodeDomain,
		func() (*media.ProcessingEmoji, error) {
			return d.mediaManager.RefreshEmoji(ctx,
				emoji,
				data,
				info,
			)
		},
	)
}

// processingEmojiSafely provides concurrency-safe processing of
// an emoji with given shortcode+domain. if a copy of the emoji is
// not already being processed, the given 'process' callback will
// be used to generate new *media.ProcessingEmoji{} instance.
func (d *Dereferencer) processEmojiSafely(
	ctx context.Context,
	shortcodeDomain string,
	process func() (*media.ProcessingEmoji, error),
) (
	emoji *gtsmodel.Emoji,
	err error,
) {

	// Acquire map lock.
	d.derefEmojisMu.Lock()

	// Ensure unlock only done once.
	unlock := d.derefEmojisMu.Unlock
	unlock = util.DoOnce(unlock)
	defer unlock()

	// Look for an existing dereference in progress.
	processing, ok := d.derefEmojis[shortcodeDomain]

	if !ok {
		// Start new processing emoji.
		processing, err = process()
		if err != nil {
			return nil, err
		}
	}

	// Unlock map.
	unlock()

	// Perform emoji load operation.
	emoji, err = processing.Load(ctx)
	if err != nil {
		err = gtserror.Newf("error loading emoji %s: %w", shortcodeDomain, err)

		// TODO: in time we should return checkable flags by gtserror.Is___()
		// which can determine if loading error should allow remaining placeholder.
	}

	// Return a COPY of emoji.
	emoji2 := new(gtsmodel.Emoji)
	*emoji2 = *emoji
	return emoji2, err
}

func (d *Dereferencer) fetchEmojis(
	ctx context.Context,
	existing []*gtsmodel.Emoji,
	emojis []*gtsmodel.Emoji, // newly dereferenced
) (
	[]*gtsmodel.Emoji,
	bool, // any changes?
	error,
) {
	// Track any changes.
	changed := false

	for i, placeholder := range emojis {
		// Look for an existing emoji with shortcode + domain.
		existing, ok := getEmojiByShortcodeDomain(existing,
			placeholder.Shortcode,
			placeholder.Domain,
		)
		if ok && existing.ID != "" {

			// Check for any emoji changes that
			// indicate we should force a refresh.
			force := emojiChanged(existing, placeholder)

			// Ensure that the existing emoji model is up-to-date and cached.
			existing, err := d.RefreshEmoji(ctx, existing, media.AdditionalEmojiInfo{

				// Set latest values from placeholder.
				URI:                  &placeholder.URI,
				ImageRemoteURL:       &placeholder.ImageRemoteURL,
				ImageStaticRemoteURL: &placeholder.ImageStaticRemoteURL,
			}, force)
			if err != nil {
				log.Errorf(ctx, "error refreshing emoji: %v", err)

				// specifically do NOT continue here,
				// we already have a model, we don't
				// want to drop it from the slice, just
				// log that an update for it failed.
			}

			// Set existing emoji.
			emojis[i] = existing
			continue
		}

		// Emojis changed!
		changed = true

		// Fetch this newly added emoji,
		// this function handles the case
		// of existing cached emojis and
		// new ones requiring dereference.
		emoji, err := d.GetEmoji(ctx,
			placeholder.Shortcode,
			placeholder.Domain,
			placeholder.ImageRemoteURL,
			media.AdditionalEmojiInfo{
				URI:                  &placeholder.URI,
				ImageRemoteURL:       &placeholder.ImageRemoteURL,
				ImageStaticRemoteURL: &placeholder.ImageStaticRemoteURL,
			},
			false,
		)
		if err != nil {
			if emoji == nil {
				log.Errorf(ctx, "error loading emoji %s: %v", placeholder.ImageRemoteURL, err)
				continue
			}

			// non-fatal error occurred during loading, still use it.
			log.Warnf(ctx, "partially loaded emoji: %v", err)
		}

		// Set updated emoji.
		emojis[i] = emoji
	}

	for i := 0; i < len(emojis); {
		if emojis[i].ID == "" {
			// Remove failed emoji populations.
			copy(emojis[i:], emojis[i+1:])
			emojis = emojis[:len(emojis)-1]
			continue
		}
		i++
	}

	return emojis, changed, nil
}
