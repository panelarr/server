// SPDX-License-Identifier: GPL-3.0-or-later

// Package cbz writes and reads comic book archives: a ZIP with the pages
// stored (not compressed, images already are) as 0001.jpg, 0002.png, …
// and a ComicInfo.xml that other readers understand.
package cbz

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"os"
	"path"
	"slices"
	"strings"
)

// ComicInfo is the ComicInfo.xml metadata (the fields other readers use).
type ComicInfo struct {
	XMLName     xml.Name `xml:"ComicInfo"`
	Title       string   `xml:"Title,omitempty"`
	Series      string   `xml:"Series,omitempty"`
	Number      string   `xml:"Number,omitempty"`
	Volume      int      `xml:"Volume,omitempty"`
	Summary     string   `xml:"Summary,omitempty"`
	Notes       string   `xml:"Notes,omitempty"`
	Year        int      `xml:"Year,omitempty"`
	Writer      string   `xml:"Writer,omitempty"`
	Penciller   string   `xml:"Penciller,omitempty"`
	Translator  string   `xml:"Translator,omitempty"`
	Genre       string   `xml:"Genre,omitempty"`
	Web         string   `xml:"Web,omitempty"`
	PageCount   int      `xml:"PageCount,omitempty"`
	LanguageISO string   `xml:"LanguageISO,omitempty"`
	Manga       string   `xml:"Manga,omitempty"`
	AgeRating   string   `xml:"AgeRating,omitempty"`
}

// Writer builds an archive in a file.
type Writer struct {
	path  string
	f     *os.File
	zw    *zip.Writer
	h     hash.Hash
	cw    *countWriter
	pages int
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Create starts an archive at path.
func Create(p string) (*Writer, error) {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a temporary file the server names
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	return &Writer{path: p, f: f, zw: zip.NewWriter(cw), h: h, cw: cw}, nil
}

// extOf picks a page file extension from an image's content type.
func extOf(contentType string) string {
	ct, _, _ := mime.ParseMediaType(contentType)
	switch ct {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/avif":
		return "avif"
	case "image/jxl":
		return "jxl"
	}
	return ""
}

// ErrNotImage is a page that isn't a known image type.
var ErrNotImage = errors.New("cbz: page is not a supported image")

// AddPage appends the next page.
func (w *Writer) AddPage(contentType string, r io.Reader) error {
	ext := extOf(contentType)
	if ext == "" {
		return fmt.Errorf("%w: %q", ErrNotImage, contentType)
	}
	w.pages++
	fw, err := w.zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("%04d.%s", w.pages, ext), Method: zip.Store})
	if err != nil {
		return err
	}
	_, err = io.Copy(fw, r)
	return err
}

// Result describes a finished archive.
type Result struct {
	Pages  int
	Bytes  int64
	SHA256 string
}

// Finish adds ComicInfo.xml, closes the archive and returns its size and
// checksum.
func (w *Writer) Finish(info ComicInfo) (Result, error) {
	info.PageCount = w.pages
	fw, err := w.zw.CreateHeader(&zip.FileHeader{Name: "ComicInfo.xml", Method: zip.Deflate})
	if err != nil {
		return Result{}, err
	}
	if _, err := io.WriteString(fw, xml.Header); err != nil {
		return Result{}, err
	}
	enc := xml.NewEncoder(fw)
	enc.Indent("", "  ")
	if err := enc.Encode(info); err != nil {
		return Result{}, err
	}
	if err := w.zw.Close(); err != nil {
		return Result{}, err
	}
	if err := w.f.Sync(); err != nil {
		return Result{}, err
	}
	if err := w.f.Close(); err != nil {
		return Result{}, err
	}
	return Result{Pages: w.pages, Bytes: w.cw.n, SHA256: hex.EncodeToString(w.h.Sum(nil))}, nil
}

// Abort discards the archive.
func (w *Writer) Abort() {
	_ = w.f.Close()
	_ = os.Remove(w.path)
}

// Archive is an archive opened for reading pages.
type Archive struct {
	pages []*zip.File
}

// Open reads an archive's directory.
func Open(r io.ReaderAt, size int64) (*Archive, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, err
	}
	a := &Archive{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || strings.HasPrefix(path.Base(f.Name), ".") {
			continue
		}
		ct := mime.TypeByExtension(path.Ext(f.Name))
		if !strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "image/svg") {
			continue // not a page (SVG can carry scripts)
		}
		a.pages = append(a.pages, f)
	}
	slices.SortFunc(a.pages, func(x, y *zip.File) int { return strings.Compare(x.Name, y.Name) })
	return a, nil
}

// Pages is the number of pages.
func (a *Archive) Pages() int { return len(a.pages) }

// Page opens page i (from 0).
func (a *Archive) Page(i int) (contentType string, r io.ReadCloser, size int64, err error) {
	if i < 0 || i >= len(a.pages) {
		return "", nil, 0, fmt.Errorf("cbz: no page %d", i)
	}
	f := a.pages[i]
	rc, err := f.Open()
	if err != nil {
		return "", nil, 0, err
	}
	return mime.TypeByExtension(path.Ext(f.Name)), rc, int64(f.UncompressedSize64), nil //nolint:gosec // page sizes fit
}
