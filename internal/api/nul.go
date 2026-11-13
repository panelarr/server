// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// nulMessage is what a client sees when its input carries a NUL.
const nulMessage = "text can't contain the NUL character (U+0000)"

// rejectNUL refuses a request whose text input carries a NUL character
// (U+0000) in a path parameter, a query parameter or anywhere in a JSON
// body (keys included). PostgreSQL can't store NUL in text or jsonb, so
// such input would otherwise fail deep in the store as a 500; checking
// every operation here keeps the handlers free of the same check.
//
// It reads and buffers the body (already limited to maxBodyBytes) and puts
// it back for the spec validator and the generated code; without body
// (uploads, which stream to their handler) it checks only the parameters.
func rejectNUL(r *http.Request, rc *chi.Context, checkBody bool) *Problem {
	for i, k := range rc.URLParams.Keys {
		if strings.ContainsRune(rc.URLParams.Values[i], 0) {
			return fieldProblem(k, nulMessage)
		}
	}
	if strings.ContainsRune(r.URL.Path, 0) {
		return NewProblem(http.StatusBadRequest, CodeInvalidRequest, nulMessage)
	}
	query := r.URL.Query()
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.ContainsRune(name, 0) {
			return NewProblem(http.StatusBadRequest, CodeInvalidRequest, nulMessage)
		}
		for _, v := range query[name] {
			if strings.ContainsRune(v, 0) {
				return fieldProblem(name, nulMessage)
			}
		}
	}
	if !checkBody || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	data, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		// Too large or unreadable: replay the error so the spec validator
		// reports it as before.
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(data), errReader{err}))
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	// A raw NUL byte is never valid JSON, so only the \u0000 escape can
	// smuggle one into a decoded string.
	if !bytes.Contains(data, []byte(`\u0000`)) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var body any
	if dec.Decode(&body) != nil {
		// Not JSON: the spec validator reports it.
		return nil
	}
	if field, ok := findNUL(body, ""); ok {
		return fieldProblem(field, nulMessage)
	}
	return nil
}

// findNUL returns the dotted path (as validationProblem names fields) of
// the first string or object key in v that contains a NUL.
func findNUL(v any, path string) (string, bool) {
	join := func(k string) string {
		if path == "" {
			return k
		}
		return path + "." + k
	}
	switch t := v.(type) {
	case string:
		return path, strings.ContainsRune(t, 0)
	case []any:
		for i, e := range t {
			if f, ok := findNUL(e, join(strconv.Itoa(i))); ok {
				return f, true
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.ContainsRune(k, 0) {
				return path, true
			}
			if f, ok := findNUL(t[k], join(k)); ok {
				return f, true
			}
		}
	}
	return "", false
}

// errReader replays a read error after the bytes read before it.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
