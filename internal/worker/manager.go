package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/pburkhalter/arrarr/internal/arrclient"
	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/librarian"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

const MaxSubmitAttempts = 5
const MaxPollDuration = 24 * time.Hour
const MaxVerifyDuration = 24 * time.Hour

type FSChecker interface {
	Exists(path string) (bool, error)
}

type pathMapper interface {
	Local(folderName string) (string, error)
}

type torboxClient interface {
	CreateUsenetDownload(ctx context.Context, filename string, nzb []byte, password string) (*torbox.CreateResp, error)
	MyList(ctx context.Context, bypassCache bool) ([]torbox.MyListItem, error)
	ControlUsenet(ctx context.Context, id int64, op string) error
	EditUsenet(ctx context.Context, id int64, p torbox.EditUsenetParams) error
	RequestUsenetDL(ctx context.Context, usenetID, fileID int64, zipLink bool) (string, error)
}

type Options struct {
	Store           *store.Store
	Torbox          torboxClient
	PathMap         pathMapper
	FS              FSChecker
	Logger          *slog.Logger
	Wake            <-chan struct{}
	DispatchEvery   time.Duration
	PollEvery       time.Duration
	VerifyEvery     time.Duration
	ReapEvery       time.Duration
	ReapOlderThan   time.Duration
	WorkerPoolSize  int

	// InstanceName is included in TorBox tags as "host:<name>" so this Arrarr
	// instance's downloads are distinguishable from other users / instances
	// sharing the same TorBox account. See PLAN.md "Tagging strategy".
	InstanceName string

	// --- v2 librarian ---
	// Librarian writes structured library entries; nil = no librarian loop runs
	// and the verifier stays responsible for COMPLETED_TORBOX → READY (v1 path).
	Librarian librarian.Writer

	// LocalVerifyBase is where /mnt/torbox is mounted in this container; passed
	// through to the librarian for symlink mode.
	LocalVerifyBase string

	// StreamingURLRefreshAfter is how long after writing a STRM we expect the
	// underlying TorBox CDN URL to remain valid. Used for refresh scheduling.
	StreamingURLRefreshAfter time.Duration

	// Sonarr / Radarr are optional canonical-naming oracles. Either may be nil;
	// the librarian falls back to release-name layout when no match is found.
	Sonarr *arrclient.Client
	Radarr *arrclient.Client

	// ArrCallbackTimeout caps each parse() call to keep the librarian loop
	// bounded if an arr is slow.
	ArrCallbackTimeout time.Duration

	// --- v2 mirror mode ---
	// When MirrorEnabled is true, periodic mylist sweeps synthesize "mirror"
	// jobs for downloads on the shared TorBox account that Arrarr didn't
	// initiate. The librarian writes library entries for them; Pushover stays
	// gated to origin='self' (so other users' downloads don't generate noise).
	MirrorEnabled      bool
	MirrorPollInterval time.Duration
}

type Manager struct {
	o    Options
	log  *slog.Logger
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
	if o.VerifyEvery == 0 {
		o.VerifyEvery = 60 * time.Second
	}
	if o.ReapEvery == 0 {
		o.ReapEvery = 5 * time.Minute
	}
	if o.ReapOlderThan == 0 {
		o.ReapOlderThan = 7 * 24 * time.Hour
	}
	if o.ArrCallbackTimeout == 0 {
		o.ArrCallbackTimeout = 5 * time.Second
	}
	if o.StreamingURLRefreshAfter == 0 {
		o.StreamingURLRefreshAfter = 5 * time.Hour
	}
	return &Manager{o: o, log: o.Logger}
}

func (m *Manager) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	loops := []func(context.Context){
		m.submitterLoop,
		m.pollerLoop,
		m.verifierLoop,
		m.librarianLoop,
		m.urlRefreshLoop,
		m.taggerRetryLoop,
		m.mirrorLoop,
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

func usefulStates() []job.State { return job.ActiveStates() }
