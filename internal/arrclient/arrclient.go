// Package arrclient is a minimal HTTP client for the Sonarr/Radarr v3 API.
// We use it as a *naming oracle* — given a release title, ask the arr to parse
// it into canonical metadata (series/season/episode for Sonarr; movie/year for
// Radarr). Used by the librarian to derive structured filenames.
//
// We intentionally do NOT mutate arr state from here. Read-only. If the arr
// is unavailable or returns no match, the librarian falls back to the original
// release name and lets the arr re-parse during its own scan.
package arrclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MediaType disambiguates which arr to call.
type MediaType string

const (
	MediaTypeSeries MediaType = "series"
	MediaTypeMovie  MediaType = "movie"
)

// ParseResult is the subset of Sonarr/Radarr's parse response we care about.
// Both arrs share most of these fields though they live under different
// container objects in the JSON ("series"+"episodes" vs "movie").
type ParseResult struct {
	MediaType   MediaType
	Title       string // canonical series or movie title
	Year        int    // movies only; 0 for series
	Season      int    // series only; 0 for movies
	Episodes    []int  // series only; multi-ep releases possible
	EpisodeName string // series only; first episode's title if available
	Quality     string // e.g. "1080p WEB-DL"
}

// HasSeasonEpisode reports whether the parse looks like a usable series result.
func (p *ParseResult) HasSeasonEpisode() bool {
	return p.MediaType == MediaTypeSeries && p.Season > 0 && len(p.Episodes) > 0
}

// HasMovie reports whether the parse looks like a usable movie result.
func (p *ParseResult) HasMovie() bool {
	return p.MediaType == MediaTypeMovie && p.Title != "" && p.Year > 0
}

// Client wraps either Sonarr or Radarr. The MediaType field selects which
// shape to expect from the JSON response.
type Client struct {
	BaseURL   string
	APIKey    string
	MediaType MediaType
	HTTP      *http.Client
}

// New constructs a client. baseURL must be the full server URL (no /api suffix).
// timeout caps each individual request; the caller controls overall context.
func New(baseURL, apiKey string, mt MediaType, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		MediaType: mt,
		HTTP:      &http.Client{Timeout: timeout},
	}
}

// ErrNoMatch is returned when the arr accepts the request but reports no match
// for the supplied release title. Callers should fall back to release-name
// parsing in this case.
var ErrNoMatch = errors.New("arr returned no parse match")

// Parse asks the arr to identify the supplied release title. The release title
// should be a typical scene-style filename, e.g.
//
//	"Twisted.Metal.S02E01.GERMAN.DL.1080p.WEB.H264-WAYNE"
//	"Hoppers.2026.GERMAN.DL.1080p.WEB.H264-MGE"
//
// On success returns a populated ParseResult. On no-match returns ErrNoMatch.
func (c *Client) Parse(ctx context.Context, releaseTitle string) (*ParseResult, error) {
	q := url.Values{}
	q.Set("title", releaseTitle)
	q.Set("apikey", c.APIKey) // both sonarr/radarr accept apikey query param
	u := c.BaseURL + "/api/v3/parse?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.APIKey) // belt + suspenders
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("arrclient: 401 unauthorized (check API key)")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("arrclient: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	switch c.MediaType {
	case MediaTypeSeries:
		return parseSonarr(body, releaseTitle)
	case MediaTypeMovie:
		return parseRadarr(body, releaseTitle)
	default:
		return nil, fmt.Errorf("arrclient: unsupported MediaType %q", c.MediaType)
	}
}

// --- Sonarr response shape ---
//
// /api/v3/parse?title=... returns:
//   { "title": "<release name>",
//     "parsedEpisodeInfo": { "seriesTitle": "...", "seasonNumber": N,
//                            "episodeNumbers": [N], "quality": { "quality": { "name": "..." } } },
//     "series": { "id": N, "title": "..." },
//     "episodes": [ { "title": "...", "seasonNumber": N, "episodeNumber": N }, ... ]
//   }
// On no match: parsedEpisodeInfo may be null, series may be missing.

type sonarrParseResp struct {
	Title  string `json:"title"`
	Parsed *struct {
		SeriesTitle    string `json:"seriesTitle"`
		SeasonNumber   int    `json:"seasonNumber"`
		EpisodeNumbers []int  `json:"episodeNumbers"`
		Quality        struct {
			Quality struct {
				Name string `json:"name"`
			} `json:"quality"`
		} `json:"quality"`
	} `json:"parsedEpisodeInfo"`
	Series *struct {
		Title string `json:"title"`
	} `json:"series"`
	Episodes []struct {
		Title         string `json:"title"`
		SeasonNumber  int    `json:"seasonNumber"`
		EpisodeNumber int    `json:"episodeNumber"`
	} `json:"episodes"`
}

func parseSonarr(body []byte, fallbackTitle string) (*ParseResult, error) {
	var r sonarrParseResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("arrclient/sonarr decode: %w", err)
	}
	if r.Parsed == nil {
		return nil, ErrNoMatch
	}
	out := &ParseResult{
		MediaType: MediaTypeSeries,
		Season:    r.Parsed.SeasonNumber,
		Episodes:  append([]int{}, r.Parsed.EpisodeNumbers...),
		Quality:   r.Parsed.Quality.Quality.Name,
	}
	if r.Series != nil && r.Series.Title != "" {
		out.Title = r.Series.Title
	} else {
		out.Title = r.Parsed.SeriesTitle
	}
	if out.Title == "" {
		out.Title = fallbackTitle
	}
	if len(r.Episodes) > 0 && r.Episodes[0].Title != "" {
		out.EpisodeName = r.Episodes[0].Title
	}
	if !out.HasSeasonEpisode() {
		return out, ErrNoMatch
	}
	return out, nil
}

// --- Radarr response shape ---
//
// /api/v3/parse?title=... returns:
//   { "title": "<release>",
//     "parsedMovieInfo": { "movieTitles": ["..."], "year": YYYY,
//                          "quality": { "quality": { "name": "..." } } },
//     "movie": { "id": N, "title": "...", "year": YYYY }
//   }

type radarrParseResp struct {
	Title  string `json:"title"`
	Parsed *struct {
		MovieTitles []string `json:"movieTitles"`
		Year        int      `json:"year"`
		Quality     struct {
			Quality struct {
				Name string `json:"name"`
			} `json:"quality"`
		} `json:"quality"`
	} `json:"parsedMovieInfo"`
	Movie *struct {
		Title string `json:"title"`
		Year  int    `json:"year"`
	} `json:"movie"`
}

func parseRadarr(body []byte, fallbackTitle string) (*ParseResult, error) {
	var r radarrParseResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("arrclient/radarr decode: %w", err)
	}
	if r.Parsed == nil {
		return nil, ErrNoMatch
	}
	out := &ParseResult{
		MediaType: MediaTypeMovie,
		Year:      r.Parsed.Year,
		Quality:   r.Parsed.Quality.Quality.Name,
	}
	if r.Movie != nil && r.Movie.Title != "" {
		out.Title = r.Movie.Title
		if r.Movie.Year > 0 {
			out.Year = r.Movie.Year
		}
	} else if len(r.Parsed.MovieTitles) > 0 {
		out.Title = r.Parsed.MovieTitles[0]
	}
	if out.Title == "" {
		out.Title = fallbackTitle
	}
	if !out.HasMovie() {
		return out, ErrNoMatch
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
