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

package media

import (
	"bytes"
	"context"
	"io"
	"slices"

	"codeberg.org/gruf/go-bytesize"
	errorsv2 "codeberg.org/gruf/go-errors/v2"
	"codeberg.org/gruf/go-runners"
	"github.com/h2non/filetype"
	"github.com/superseriousbusiness/gotosocial/internal/config"
	"github.com/superseriousbusiness/gotosocial/internal/gtscontext"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/regexes"
	"github.com/superseriousbusiness/gotosocial/internal/storage"
	"github.com/superseriousbusiness/gotosocial/internal/uris"
	"github.com/superseriousbusiness/gotosocial/internal/util"
)

// ProcessingEmoji represents an emoji currently processing. It exposes
// various functions for retrieving data from the process.
type ProcessingEmoji struct {
	emoji     *gtsmodel.Emoji   // processing emoji details
	newPathID string            // new emoji path ID to use when being refreshed
	dataFn    DataFunc          // load-data function, returns media stream
	done      bool              // done is set when process finishes with non ctx canceled type error
	proc      runners.Processor // proc helps synchronize only a singular running processing instance
	err       error             // error stores permanent error value when done
	mgr       *Manager          // mgr instance (access to db / storage)
}

// ID returns the ID of the underlying emoji.
func (p *ProcessingEmoji) ID() string {
	return p.emoji.ID // immutable, safe outside mutex.
}

// LoadEmoji blocks until the static and fullsize image has been processed, and then returns the completed emoji.
func (p *ProcessingEmoji) Load(ctx context.Context) (*gtsmodel.Emoji, error) {
	emoji, done, err := p.load(ctx)
	if !done {
		// On a context-canceled error (marked as !done), requeue for loading.
		p.mgr.state.Workers.Dereference.Queue.Push(func(ctx context.Context) {
			if _, _, err := p.load(ctx); err != nil {
				log.Errorf(ctx, "error loading emoji: %v", err)
			}
		})
	}
	return emoji, err
}

// load is the package private form of load() that is wrapped to catch context canceled.
func (p *ProcessingEmoji) load(ctx context.Context) (
	emoji *gtsmodel.Emoji,
	done bool,
	err error,
) {
	err = p.proc.Process(func() error {
		if done = p.done; done {
			// Already proc'd.
			return p.err
		}

		defer func() {
			// This is only done when ctx NOT cancelled.
			done = (err == nil || !errorsv2.IsV2(err,
				context.Canceled,
				context.DeadlineExceeded,
			))

			if !done {
				return
			}

			// Anything from here, we
			// need to ensure happens
			// (i.e. no ctx canceled).
			ctx = gtscontext.WithValues(
				context.Background(),
				ctx, // values
			)

			// On error, clean
			// downloaded files.
			if err != nil {
				p.cleanup(ctx)
			}

			if !done {
				return
			}

			// Update with latest details, whatever happened.
			e := p.mgr.state.DB.UpdateEmoji(ctx, p.emoji)
			if e != nil {
				log.Errorf(ctx, "error updating emoji in db: %v", e)
			}

			// Store final values.
			p.done = true
			p.err = err
		}()

		// Attempt to store media and calculate
		// full-size media attachment details.
		//
		// This will update p.emoji as it goes.
		if err = p.store(ctx); err != nil {
			return err
		}

		// Finish processing by reloading media into
		// memory to get dimension and generate a thumb.
		//
		// This will update p.emoji as it goes.
		if err = p.finish(ctx); err != nil {
			return err //nolint:revive
		}

		return nil
	})
	emoji = p.emoji
	return
}

// store calls the data function attached to p if it hasn't been called yet,
// and updates the underlying attachment fields as necessary. It will then stream
// bytes from p's reader directly into storage so that it can be retrieved later.
func (p *ProcessingEmoji) store(ctx context.Context) error {
	// Load media from provided data fun
	rc, sz, err := p.dataFn(ctx)
	if err != nil {
		return gtserror.Newf("error executing data function: %w", err)
	}

	defer func() {
		// Ensure data reader gets closed on return.
		if err := rc.Close(); err != nil {
			log.Errorf(ctx, "error closing data reader: %v", err)
		}
	}()

	var maxSize bytesize.Size

	if p.emoji.IsLocal() {
		// this is a local emoji upload
		maxSize = config.GetMediaEmojiLocalMaxSize()
	} else {
		// this is a remote incoming emoji
		maxSize = config.GetMediaEmojiRemoteMaxSize()
	}

	// Check that provided size isn't beyond max. We check beforehand
	// so that we don't attempt to stream the emoji into storage if not needed.
	if sz > 0 && sz > int64(maxSize) {
		sz := bytesize.Size(sz) // improves log readability
		return gtserror.Newf("given emoji size %s greater than max allowed %s", sz, maxSize)
	}

	// Prepare to read bytes from
	// file header or magic number.
	fileSize := int(sz)
	hdrBuf := newHdrBuf(fileSize)

	// Read into buffer as much as possible.
	//
	// UnexpectedEOF means we couldn't read up to the
	// given size, but we may still have read something.
	//
	// EOF means we couldn't read anything at all.
	//
	// Any other error likely means the connection messed up.
	//
	// In other words, rather counterintuitively, we
	// can only proceed on no error or unexpected error!
	n, err := io.ReadFull(rc, hdrBuf)
	if err != nil {
		if err != io.ErrUnexpectedEOF {
			return gtserror.Newf("error reading first bytes of incoming media: %w", err)
		}

		// Initial file size was misreported, so we didn't read
		// fully into hdrBuf. Reslice it to the size we did read.
		hdrBuf = hdrBuf[:n]
		fileSize = n
		p.emoji.ImageFileSize = fileSize
	}

	// Parse file type info from header buffer.
	// This should only ever error if the buffer
	// is empty (ie., the attachment is 0 bytes).
	info, err := filetype.Match(hdrBuf)
	if err != nil {
		return gtserror.Newf("error parsing file type: %w", err)
	}

	// Ensure supported emoji img type.
	if !slices.Contains(SupportedEmojiMIMETypes, info.MIME.Value) {
		return gtserror.Newf("unsupported emoji filetype: %s", info.Extension)
	}

	// Recombine header bytes with remaining stream
	r := io.MultiReader(bytes.NewReader(hdrBuf), rc)

	var pathID string
	if p.newPathID != "" {
		// This is a refreshed emoji with a new
		// path ID that this will be stored under.
		pathID = p.newPathID
	} else {
		// This is a new emoji, simply use provided ID.
		pathID = p.emoji.ID
	}

	// Determine instance account ID from generated image static path.
	instanceAccID, ok := getInstanceAccountID(p.emoji.ImageStaticPath)
	if !ok {
		return gtserror.Newf("invalid emoji static path; no instance account id: %s", p.emoji.ImageStaticPath)
	}

	// Calculate final media attachment file path.
	p.emoji.ImagePath = uris.StoragePathForAttachment(
		instanceAccID,
		string(TypeEmoji),
		string(SizeOriginal),
		pathID,
		info.Extension,
	)

	// File shouldn't already exist in storage at this point,
	// but we do a check as it's worth logging / cleaning up.
	if have, _ := p.mgr.state.Storage.Has(ctx, p.emoji.ImagePath); have {
		log.Warnf(ctx, "emoji already exists at: %s", p.emoji.ImagePath)

		// Attempt to remove existing emoji at storage path (might be broken / out-of-date)
		if err := p.mgr.state.Storage.Delete(ctx, p.emoji.ImagePath); err != nil {
			return gtserror.Newf("error removing emoji %s from storage: %v", p.emoji.ImagePath, err)
		}
	}

	// Write the final image reader stream to our storage.
	sz, err = p.mgr.state.Storage.PutStream(ctx, p.emoji.ImagePath, r)
	if err != nil {
		return gtserror.Newf("error writing emoji to storage: %w", err)
	}

	// Perform final size check in case none was
	// given previously, or size was mis-reported.
	// (error here will later perform p.cleanup()).
	if sz > int64(maxSize) {
		sz := bytesize.Size(sz) // improves log readability
		return gtserror.Newf("written emoji size %s greater than max allowed %s", sz, maxSize)
	}

	// Fill in remaining emoji data now it's stored.
	p.emoji.ImageURL = uris.URIForAttachment(
		instanceAccID,
		string(TypeEmoji),
		string(SizeOriginal),
		pathID,
		info.Extension,
	)
	p.emoji.ImageContentType = info.MIME.Value
	p.emoji.ImageFileSize = int(sz)
	p.emoji.Cached = util.Ptr(true)

	return nil
}

func (p *ProcessingEmoji) finish(ctx context.Context) error {
	// Get a stream to the original file for further processing.
	rc, err := p.mgr.state.Storage.GetStream(ctx, p.emoji.ImagePath)
	if err != nil {
		return gtserror.Newf("error loading file from storage: %w", err)
	}
	defer rc.Close()

	// Decode the image from storage.
	staticImg, err := decodeImage(rc)
	if err != nil {
		return gtserror.Newf("error decoding image: %w", err)
	}

	// staticImg should be in-memory by
	// now so we're done with storage.
	if err := rc.Close(); err != nil {
		return gtserror.Newf("error closing file: %w", err)
	}

	// Static img shouldn't exist in storage at this point,
	// but we do a check as it's worth logging / cleaning up.
	if have, _ := p.mgr.state.Storage.Has(ctx, p.emoji.ImageStaticPath); have {
		log.Warnf(ctx, "static emoji already exists at: %s", p.emoji.ImageStaticPath)

		// Attempt to remove existing thumbnail (might be broken / out-of-date).
		if err := p.mgr.state.Storage.Delete(ctx, p.emoji.ImageStaticPath); err != nil {
			return gtserror.Newf("error removing static emoji %s from storage: %v", p.emoji.ImageStaticPath, err)
		}
	}

	// Create emoji PNG encoder stream.
	enc := staticImg.ToPNG()

	// Stream-encode the PNG static emoji image into our storage driver.
	sz, err := p.mgr.state.Storage.PutStream(ctx, p.emoji.ImageStaticPath, enc)
	if err != nil {
		return gtserror.Newf("error stream-encoding static emoji to storage: %w", err)
	}

	// Set final written thumb size.
	p.emoji.ImageStaticFileSize = int(sz)

	return nil
}

// cleanup will remove any traces of processing emoji from storage,
// and perform any other necessary cleanup steps after failure.
func (p *ProcessingEmoji) cleanup(ctx context.Context) {
	var err error

	if p.emoji.ImagePath != "" {
		// Ensure emoji file at path is deleted from storage.
		err = p.mgr.state.Storage.Delete(ctx, p.emoji.ImagePath)
		if err != nil && !storage.IsNotFound(err) {
			log.Errorf(ctx, "error deleting %s: %v", p.emoji.ImagePath, err)
		}
	}

	if p.emoji.ImageStaticPath != "" {
		// Ensure emoji static file at path is deleted from storage.
		err = p.mgr.state.Storage.Delete(ctx, p.emoji.ImageStaticPath)
		if err != nil && !storage.IsNotFound(err) {
			log.Errorf(ctx, "error deleting %s: %v", p.emoji.ImageStaticPath, err)
		}
	}

	// Ensure marked as not cached.
	p.emoji.Cached = util.Ptr(false)
}

// getInstanceAccountID determines the instance account ID from
// emoji static image storage path. returns false on failure.
func getInstanceAccountID(staticPath string) (string, bool) {
	matches := regexes.FilePath.FindStringSubmatch(staticPath)
	if len(matches) < 2 {
		return "", false
	}
	return matches[1], true
}
