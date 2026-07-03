package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

const MaxSubmitAttempts = 5
const MaxPollDuration = 24 * time.Hour

// MaxMissingDuration bounds how long a job may be absent from TorBox's mylist
// before we treat it as lost and fail it. TorBox silently drops downloads it
// can't complete (e.g. German-dubbed usenet releases with missing articles),
// leaving them at 0 bytes and never emitting a terminal download_state. Without
// this, such a job would linger in DOWNLOADING until MaxPollDuration (24h),
// holding a TorBox active-download slot the whole time and starving the NEW
// backlog behind the account's concurrency limit.
const MaxMissingDuration = 15 * time.Minute

// torboxClient is the slice of torbox.Client the worker loops actually use.
// Kept narrow so tests can stand up in-memory fakes.
type torboxClient interface {
	CreateUsenetDownload(ctx context.Context, filename string, nzb []byte, password string) (*torbox.CreateResp, error)
	CreateTorrentFromFile(ctx context.Context, filename string, torrentBlob []byte, p torbox.CreateTorrentParams) (*torbox.CreateResp, error)
	CreateTorrentFromMagnet(ctx context.Context, magnet string, p torbox.CreateTorrentParams) (*torbox.CreateResp, error)
	MyList(ctx context.Context, bypassCache bool) ([]torbox.MyListItem, error)
	MyListTorrents(ctx context.Context, bypassCache bool) ([]torbox.MyListItem, error)
	RequestUsenetDL(ctx context.Context, usenetID, fileID int64, zipLink bool) (string, error)
	RequestTorrentDL(ctx context.Context, torrentID, fileID int64, zipLink bool) (string, error)
	ControlUsenet(ctx context.Context, id int64, op string) error
	ControlTorrent(ctx context.Context, id int64, op string) error
}

type Options struct {
	Store          *store.Store
	Torbox         torboxClient
	Logger         *slog.Logger
	Wake           <-chan struct{}
	DispatchEvery  time.Duration
	PollEvery      time.Duration
	ReapEvery      time.Duration
	ReapOlderThan  time.Duration
	WorkerPoolSize int

	// Puller pulls completed TorBox items to local disk. Required — the puller
	// owns the COMPLETED_TORBOX → READY transition in v3.
	Puller    *Puller
	PullEvery time.Duration
}

type Manager struct {
	o   Options
	log *slog.Logger
}

func New(o Options) *Manager {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.WorkerPoolSize < 1 {
		o.WorkerPoolSize = 4
	}
	if o.DispatchEvery == 0 {
		o.DispatchEvery = 10 * time.Second
	}
	if o.PollEvery == 0 {
		o.PollEvery = 30 * time.Second
	}
	if o.ReapEvery == 0 {
		o.ReapEvery = 5 * time.Minute
	}
	if o.ReapOlderThan == 0 {
		o.ReapOlderThan = 7 * 24 * time.Hour
	}
	if o.PullEvery == 0 {
		o.PullEvery = 15 * time.Second
	}
	return &Manager{o: o, log: o.Logger}
}

func (m *Manager) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	loops := []func(context.Context){
		m.submitterLoop,
		m.pollerLoop,
		m.pullerLoop,
		m.reaperLoop,
	}
	for _, fn := range loops {
		wg.Add(1)
		go func(f func(context.Context)) {
			defer wg.Done()
			f(ctx)
		}(fn)
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

func describe(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *torbox.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Error()
	}
	return err.Error()
}

