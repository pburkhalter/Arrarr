package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
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
	return &Manager{o: o, log: o.Logger}
}

func (m *Manager) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	loops := []func(context.Context){
		m.submitterLoop,
		m.pollerLoop,
		m.verifierLoop,
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
