package torbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
)

// CreateTorrentParams holds the optional fields accepted by /torrents/createtorrent.
type CreateTorrentParams struct {
	Seed     int // 0=auto, 1=seed, 2=don't-seed (TorBox spec)
	AllowZip bool
	Name     string
	AsQueued bool
}

// CreateTorrentFromFile uploads a .torrent file. Returns the queued/active id like CreateUsenetDownload.
// Shares the createLimiter with CreateUsenetDownload — TorBox's 60/hour
// create-endpoint ceiling counts them together.
func (c *Client) CreateTorrentFromFile(ctx context.Context, filename string, torrent []byte, p CreateTorrentParams) (*CreateResp, error) {
	if err := c.waitCreate(ctx); err != nil {
		return nil, err
	}
	body, contentType, err := buildTorrentMultipart(filename, torrent, "", p)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/torrents/createtorrent", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	c.auth(req)

	var out CreateResp
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateTorrentFromMagnet submits a magnet URI.
func (c *Client) CreateTorrentFromMagnet(ctx context.Context, magnet string, p CreateTorrentParams) (*CreateResp, error) {
	if err := c.waitCreate(ctx); err != nil {
		return nil, err
	}
	// TorBox accepts multipart even for magnet — keeps one code path.
	body, contentType, err := buildTorrentMultipart("", nil, magnet, p)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/torrents/createtorrent", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	c.auth(req)

	var out CreateResp
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MyListTorrents returns the user's torrent list. Schema is shared with usenet MyListItem.
func (c *Client) MyListTorrents(ctx context.Context, bypassCache bool) ([]MyListItem, error) {
	q := url.Values{}
	if bypassCache {
		q.Set("bypass_cache", "true")
	}
	u := c.BaseURL + "/torrents/mylist"
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)
	var out []MyListItem
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RequestTorrentDL fetches a presigned CDN URL for a single file.
// fileID=0 + zipLink=true returns a zip of all files.
func (c *Client) RequestTorrentDL(ctx context.Context, torrentID, fileID int64, zipLink bool) (string, error) {
	q := url.Values{}
	q.Set("token", c.APIKey)
	q.Set("torrent_id", fmt.Sprintf("%d", torrentID))
	if fileID > 0 {
		q.Set("file_id", fmt.Sprintf("%d", fileID))
	}
	if zipLink {
		q.Set("zip_link", "true")
	}
	q.Set("redirect", "false")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/torrents/requestdl?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	c.auth(req)

	var dlURL string
	if err := c.do(req, &dlURL); err != nil {
		return "", err
	}
	return dlURL, nil
}

// ControlTorrent pauses/resumes/deletes/reannounces a torrent.
func (c *Client) ControlTorrent(ctx context.Context, id int64, operation string) error {
	body := strings.NewReader(fmt.Sprintf(`{"torrent_id":%d,"operation":%q}`, id, operation))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/torrents/controltorrent", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	return c.do(req, nil)
}

// buildTorrentMultipart builds the /torrents/createtorrent body. Pass torrent
// bytes OR a magnet string (not both). Uses a hand-rolled MIME header for the
// file part to avoid the charset parameter that mime/multipart's
// CreateFormFile attaches — same reason as buildNZBMultipart.
func buildTorrentMultipart(filename string, torrent []byte, magnet string, p CreateTorrentParams) (io.Reader, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	if len(torrent) > 0 {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="file"; filename=%q`, sanitizeTorrentFilename(filename)))
		hdr.Set("Content-Type", "application/x-bittorrent")
		part, err := w.CreatePart(hdr)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(torrent); err != nil {
			return nil, "", err
		}
	}
	if magnet != "" {
		if err := w.WriteField("magnet", magnet); err != nil {
			return nil, "", err
		}
	}
	if err := w.WriteField("seed", strconv.Itoa(p.Seed)); err != nil {
		return nil, "", err
	}
	if p.AllowZip {
		if err := w.WriteField("allow_zip", "true"); err != nil {
			return nil, "", err
		}
	}
	if p.Name != "" {
		if err := w.WriteField("name", p.Name); err != nil {
			return nil, "", err
		}
	}
	if p.AsQueued {
		if err := w.WriteField("as_queued", "true"); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &buf, w.FormDataContentType(), nil
}

func sanitizeTorrentFilename(s string) string {
	if s == "" {
		return "upload.torrent"
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '"' || r == '\\' || r == '\n' || r == '\r' {
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return "upload.torrent"
	}
	return string(out)
}
