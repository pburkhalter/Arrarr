package arrclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSonarrParseHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("title"); !strings.Contains(got, "Twisted.Metal") {
			t.Errorf("title query=%q", got)
		}
		if r.Header.Get("X-Api-Key") != "k" {
			t.Errorf("missing api key header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"title": "Twisted.Metal.S02E01.GERMAN.DL.1080p.WEB.H264-WAYNE",
			"parsedEpisodeInfo": {
				"seriesTitle": "Twisted Metal",
				"seasonNumber": 2,
				"episodeNumbers": [1],
				"quality": { "quality": { "name": "WEBDL-1080p" } }
			},
			"series": { "id": 5, "title": "Twisted Metal" },
			"episodes": [ { "title": "What's New, Pussycat", "seasonNumber": 2, "episodeNumber": 1 } ]
		}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "k", MediaTypeSeries, time.Second)
	got, err := c.Parse(context.Background(), "Twisted.Metal.S02E01.GERMAN.DL.1080p.WEB.H264-WAYNE")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Twisted Metal" || got.Season != 2 || len(got.Episodes) != 1 || got.Episodes[0] != 1 {
		t.Errorf("got=%+v", got)
	}
	if got.EpisodeName != "What's New, Pussycat" {
		t.Errorf("episode name=%q", got.EpisodeName)
	}
	if got.Quality != "WEBDL-1080p" {
		t.Errorf("quality=%q", got.Quality)
	}
	if !got.HasSeasonEpisode() {
		t.Error("HasSeasonEpisode should be true")
	}
}

func TestSonarrParseNoMatchReturnsErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"title":"junk","parsedEpisodeInfo":null}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "k", MediaTypeSeries, time.Second)
	got, err := c.Parse(context.Background(), "junk.xyz")
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("err=%v want ErrNoMatch", err)
	}
	if got != nil {
		t.Errorf("expected nil result, got %+v", got)
	}
}

func TestSonarrPartialMatchSeriesOnlyTreatedAsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"parsedEpisodeInfo": {
				"seriesTitle": "Some Series",
				"seasonNumber": 0,
				"episodeNumbers": []
			}
		}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "k", MediaTypeSeries, time.Second)
	got, err := c.Parse(context.Background(), "Some.Series.NoEpInfo")
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("err=%v want ErrNoMatch", err)
	}
	// We still get the partial result back so callers can choose to use the title
	if got == nil || got.Title != "Some Series" {
		t.Errorf("partial result missing: %+v", got)
	}
}

func TestRadarrParseHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"title": "Hoppers.2026.GERMAN.DL.1080p.WEB.H264-MGE",
			"parsedMovieInfo": {
				"movieTitles": ["Hoppers"],
				"year": 2026,
				"quality": { "quality": { "name": "WEBDL-1080p" } }
			},
			"movie": { "id": 9, "title": "Hoppers", "year": 2026 }
		}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "k", MediaTypeMovie, time.Second)
	got, err := c.Parse(context.Background(), "Hoppers.2026.GERMAN.DL.1080p.WEB.H264-MGE")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Hoppers" || got.Year != 2026 || got.Quality != "WEBDL-1080p" {
		t.Errorf("got=%+v", got)
	}
	if !got.HasMovie() {
		t.Error("HasMovie should be true")
	}
}

func TestRadarrFallsBackToParsedMovieInfoWhenMovieMissing(t *testing.T) {
	// arr returns parse info but no resolved movie (e.g. unmonitored)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"parsedMovieInfo": {
				"movieTitles": ["Project Hail Mary"],
				"year": 2026,
				"quality": { "quality": { "name": "WEBDL-1080p" } }
			}
		}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "k", MediaTypeMovie, time.Second)
	got, err := c.Parse(context.Background(), "Project.Hail.Mary.2026.WEB-DL")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Project Hail Mary" {
		t.Errorf("title=%q want Project Hail Mary", got.Title)
	}
}

func TestParseAuthFailureSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"Unauthorized"}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "wrong", MediaTypeSeries, time.Second)
	_, err := c.Parse(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err=%v, expected 401-related", err)
	}
}
