package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// handler is our HTTP handler. It defers most work to the controler and
// handles muxing, response headers, etc.
type handler struct {
	ctrl *controller
	http *http.Client
}

// newHandler creates a new handler.
func newHandler(ctrl *controller) *handler {
	h := &handler{
		ctrl: ctrl,
		http: &http.Client{},
	}
	return h
}

// newMux registers a handler's routes on a new mux.
func newMux(h *handler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/work/{foreignID}", h.getWorkID)
	mux.HandleFunc("/book/{foreignEditionID}", h.getBookID)
	mux.HandleFunc("/book/bulk", h.bulkBook)
	mux.HandleFunc("/author/{foreignAuthorID}", h.getAuthorID)
	mux.HandleFunc("/author/changed", h.getAuthorChanged)

	// Default handler returns 404.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	return mux
}

// TooManyRequests -> WaitUntilRetry will respect Retry-After (seconds) header
// NotFound -> raises
// BadRequest -> raises

// bulkBook is sent as a POST request which isn't cachable. We immediately
// redirect to GET with query params so it can be cached.
//
// We then issue issue individual `/book/{id}` sub-requests in case they have
// previously been cached.
func (h *handler) bulkBook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var ids []int64

	// If this is a POST, redirect to a GET with query params so the result can
	// be cached.
	if r.Method == http.MethodPost {
		err := json.NewDecoder(r.Body).Decode(&ids)
		if err != nil {
			h.error(w, errors.Join(err, errBadRequest))
			return
		}
		if len(ids) == 0 {
			h.error(w, errMissingIDs)
			return
		}

		query := url.Values{}
		url := url.URL{Path: r.URL.Path}
		for _, id := range ids {
			query.Add("id", fmt.Sprint(id))
		}

		url.RawQuery = query.Encode()

		log(ctx).Debug("redirecting", "url", url.String())
		http.Redirect(w, r, url.String(), http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}

	// Parse query params.
	for _, idStr := range r.URL.Query()["id"] {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			h.error(w, errors.Join(err, errBadRequest))
			return
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		h.error(w, errMissingIDs)
		return
	}

	result := bulkBookResource{
		Works:   []workResource{},
		Series:  []seriesResource{},
		Authors: []authorResource{},
	}

	mu := sync.Mutex{}
	wg := sync.WaitGroup{}

	for _, id := range ids {
		wg.Add(1)

		go func(foreignBookID int64) {
			defer wg.Done()

			log(ctx).Debug("looking for book", "id", foreignBookID)

			scheme := "http"
			if r.URL.Scheme != "" {
				scheme = r.URL.Scheme
			}
			url := fmt.Sprintf("%s://%s/book/%d", scheme, r.Host, foreignBookID)

			req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
			resp, err := h.http.Do(req)
			if err != nil {
				fmt.Println("Problem fetching", r.URL.String(), err.Error())
				return // Ignore the error.
			}
			defer func() { _ = resp.Body.Close() }()

			var workRsc workResource
			err = json.NewDecoder(resp.Body).Decode(&workRsc)
			if err != nil {
				return // Ignore the error.
			}

			mu.Lock()
			defer mu.Unlock()

			result.Works = append(result.Works, workRsc)
			result.Series = []seriesResource{}

			// Authors needs to be de-duped. We have at most a handful so this is fine.
			for _, a := range result.Authors {
				if a.ForeignID == workRsc.Authors[0].ForeignID {
					return
				}
			}
			result.Authors = append(result.Authors, workRsc.Authors...)
		}(id)
	}

	wg.Wait()

	// Sort works by rating count.
	slices.SortFunc(result.Works, func(left, right workResource) int {
		return -cmp.Compare[int64](left.Books[0].RatingCount, right.Books[0].RatingCount)
	})

	cacheFor(w, 24*time.Hour, true)
	_ = json.NewEncoder(w).Encode(result)
}

// getWorkID handles /work/{id}
//
// Upstream is /work/{workID} and this redirects to /book/show/{id}.
func (h *handler) getWorkID(w http.ResponseWriter, r *http.Request) {
	workID, err := strconv.ParseInt(strings.Replace(r.URL.Path, "/work/", "", 1), 10, 64)
	if err != nil {
		h.error(w, errors.Join(err, errBadRequest))
		return
	}

	out, err := h.ctrl.GetWork(r.Context(), workID)
	if err != nil {
		h.error(w, err)
		return
	}

	cacheFor(w, _workTTL, false)
	w.WriteHeader(http.StatusOK) // TODO: Client expects this to redirect to /book/{id}.
	_, _ = w.Write(out)
}

// cacheFor sets cache response headers. s-maxage controls CDN cache time; we
// default to an hour expiry for clients.
func cacheFor(w http.ResponseWriter, d time.Duration, varyParams bool) {
	w.Header().Add("Cache-Control", fmt.Sprintf("public, s-maxage=%d, max-age=3600", int(d.Seconds())))
	w.Header().Add("Vary", "Content-Type,Accept-Encoding") // Ignore headers like User-Agent, etc.
	w.Header().Add("Content-Type", "application/json")
	// w.Header().Add("Content-Encoding", "gzip")

	if !varyParams {
		// Ignore query params when serving cached responses.
		w.Header().Add("No-Vary-Search", "params")
	}
}

// getBookID handles /book/{id}. It does so by loading upstream /book/show/{id}
// and parsing out embedded metadata. This is fragile and should eventually
// consume the graphql endpoint directly.
//
// Importantly, the client expects this to always return a redirect -- either
// to an author or a work. The work returned is then expected to be "fat" with
// all editions of the work attached to it. This is very large!
//
// TODO: Return a redirect but don't respect it when we call it ourselves.
//
// TODO: This endpoint returns a WorkResource?? Seems like it should return a BookResource
func (h *handler) getBookID(w http.ResponseWriter, r *http.Request) {
	bookID, err := strconv.ParseInt(strings.Replace(r.URL.Path, "/book/", "", 1), 10, 64)
	if err != nil {
		h.error(w, errors.Join(err, errBadRequest))
		return
	}

	out, err := h.ctrl.GetBook(r.Context(), bookID)
	if err != nil {
		h.error(w, err)
		return
	}

	cacheFor(w, _editionTTL, false)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// getAuthorID handles /author/{id} by loading upstream /book/show/{id}
// and parsing out embedded metadata.
func (h *handler) getAuthorID(w http.ResponseWriter, r *http.Request) {
	authorID, err := strconv.ParseInt(strings.Replace(r.URL.Path, "/author/", "", 1), 10, 64)
	if err != nil {
		h.error(w, errors.Join(err, errBadRequest))
		return
	}

	out, err := h.ctrl.GetAuthor(r.Context(), authorID)
	if err != nil {
		h.error(w, err)
		return
	}

	cacheFor(w, _authorTTL, false)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// getAuthorChanged handles the `/author/changed?since={datetime}` endpoint.
//
// Normally this would return IDs for _all_ authors updated since the given
// timestamp -- not just the authors in your library. The query param makes
// this uncachable and it's an expensive operation, so we return nothing and
// force the client to no-op.
//
// As a result, the client will periodically re-query `/author/{id}`:
//   - At least once every 30 days.
//   - Not more than every 12 hours.
//   - At least every 2 days if the author is "continuing."
//   - Every day if they released a book in the past 30 days, maybe to pick up
//     newer ratings? Unclear.
//
// These will hit cached entries, and client will pick up newer data gradually
// as entries become invalidated.
func (h *handler) getAuthorChanged(w http.ResponseWriter, _ *http.Request) {
	cacheFor(w, 7*24*time.Hour, false)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"Limitted": true, "Ids": []}`))
}

func (*handler) error(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var s statusErr
	if errors.As(err, &s) {
		status = s.Status()
	}
	http.Error(w, err.Error(), status)
}
