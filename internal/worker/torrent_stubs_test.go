package worker

import (
	"context"

	"github.com/pburkhalter/arrarr/internal/torbox"
)

// v3 added torrent methods to the worker's torboxClient interface. The pre-v3
// fake clients are usenet-only; these stubs let them satisfy the interface
// without anyone having to touch every test file. Tests that exercise the
// torrent path must define their own client.

func (t *taggerFakeTorbox) CreateTorrentFromFile(_ context.Context, _ string, _ []byte, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (t *taggerFakeTorbox) CreateTorrentFromMagnet(_ context.Context, _ string, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (t *taggerFakeTorbox) MyListTorrents(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (t *taggerFakeTorbox) RequestTorrentDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}
func (t *taggerFakeTorbox) ControlTorrent(_ context.Context, _ int64, _ string) error { return nil }

func (f *fakeTorbox) CreateTorrentFromFile(_ context.Context, _ string, _ []byte, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (f *fakeTorbox) CreateTorrentFromMagnet(_ context.Context, _ string, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (f *fakeTorbox) MyListTorrents(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (f *fakeTorbox) RequestTorrentDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}
func (f *fakeTorbox) ControlTorrent(_ context.Context, _ int64, _ string) error { return nil }

func (r *rateLimitedTorbox) CreateTorrentFromFile(_ context.Context, _ string, _ []byte, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (r *rateLimitedTorbox) CreateTorrentFromMagnet(_ context.Context, _ string, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (r *rateLimitedTorbox) MyListTorrents(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (r *rateLimitedTorbox) RequestTorrentDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}
func (r *rateLimitedTorbox) ControlTorrent(_ context.Context, _ int64, _ string) error { return nil }

func (t *nameFallbackTorbox) CreateTorrentFromFile(_ context.Context, _ string, _ []byte, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (t *nameFallbackTorbox) CreateTorrentFromMagnet(_ context.Context, _ string, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (t *nameFallbackTorbox) MyListTorrents(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (t *nameFallbackTorbox) RequestTorrentDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}
func (t *nameFallbackTorbox) ControlTorrent(_ context.Context, _ int64, _ string) error { return nil }

func (m *mirrorTorbox) CreateTorrentFromFile(_ context.Context, _ string, _ []byte, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (m *mirrorTorbox) CreateTorrentFromMagnet(_ context.Context, _ string, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (m *mirrorTorbox) MyListTorrents(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (m *mirrorTorbox) RequestTorrentDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}
func (m *mirrorTorbox) ControlTorrent(_ context.Context, _ int64, _ string) error { return nil }

func (r *recordingTorbox) CreateTorrentFromFile(_ context.Context, _ string, _ []byte, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (r *recordingTorbox) CreateTorrentFromMagnet(_ context.Context, _ string, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (r *recordingTorbox) MyListTorrents(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (r *recordingTorbox) RequestTorrentDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}
func (r *recordingTorbox) ControlTorrent(_ context.Context, _ int64, _ string) error { return nil }

func (u *urlRefreshTorbox) CreateTorrentFromFile(_ context.Context, _ string, _ []byte, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (u *urlRefreshTorbox) CreateTorrentFromMagnet(_ context.Context, _ string, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (u *urlRefreshTorbox) MyListTorrents(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (u *urlRefreshTorbox) RequestTorrentDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}
func (u *urlRefreshTorbox) ControlTorrent(_ context.Context, _ int64, _ string) error { return nil }
