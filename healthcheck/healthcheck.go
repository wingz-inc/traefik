package healthcheck

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/containous/traefik/log"
	"github.com/containous/traefik/safe"
	"github.com/vulcand/oxy/roundrobin"
)

var singleton *HealthCheck
var once sync.Once

// GetHealthCheck Get HealtchCheck Singleton
func GetHealthCheck() *HealthCheck {
	once.Do(func() {
		singleton = newHealthCheck()
	})
	return singleton
}

// Options are the public health check options.
type Options struct {
	URL      string
	Interval time.Duration
	LB       LoadBalancer
}

func (opt Options) String() string {
	return fmt.Sprintf("[URL: %s Interval: %s]", opt.URL, opt.Interval)
}

// BackendHealthCheck HealthCheck configuration for a backend
type BackendHealthCheck struct {
	Options
	disabledURLs   []*url.URL
	requestTimeout time.Duration
}

//HealthCheck struct
type HealthCheck struct {
	Backends map[string]*BackendHealthCheck
	cancel   context.CancelFunc
	// wg is for synchronization during testing only.
	wg sync.WaitGroup
}

// LoadBalancer includes functionality for load-balancing management.
type LoadBalancer interface {
	RemoveServer(u *url.URL) error
	UpsertServer(u *url.URL, options ...roundrobin.ServerOption) error
	Servers() []*url.URL
}

func newHealthCheck() *HealthCheck {
	return &HealthCheck{
		Backends: make(map[string]*BackendHealthCheck),
		wg:       sync.WaitGroup{},
	}
}

// NewBackendHealthCheck Instantiate a new BackendHealthCheck
func NewBackendHealthCheck(options Options) *BackendHealthCheck {
	return &BackendHealthCheck{
		Options:        options,
		requestTimeout: 5 * time.Second,
	}
}

//SetBackendsConfiguration set backends configuration
func (hc *HealthCheck) SetBackendsConfiguration(parentCtx context.Context, backends map[string]*BackendHealthCheck) {
	hc.Backends = backends
	if hc.cancel != nil {
		hc.cancel()
	}
	ctx, cancel := context.WithCancel(parentCtx)
	hc.cancel = cancel
	hc.execute(ctx)
}

func (hc *HealthCheck) execute(ctx context.Context) {
	for backendID, backend := range hc.Backends {
		currentBackend := backend
		currentBackendID := backendID
		safe.Go(func() {
			hc.wg.Add(1)
			defer hc.wg.Done()
			log.Debugf("Initial healthcheck for currentBackend %s ", currentBackendID)
			checkBackend(currentBackend)
			ticker := time.NewTicker(currentBackend.Interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					log.Debugf("Stopping all current Healthcheck goroutines")
					return
				case <-ticker.C:
					log.Debugf("Refreshing healthcheck for currentBackend %s ", currentBackendID)
					checkBackend(currentBackend)
				}
			}
		})
	}
}

func checkBackend(currentBackend *BackendHealthCheck) {
	enabledURLs := currentBackend.LB.Servers()
	var newDisabledURLs []*url.URL
	for _, url := range currentBackend.disabledURLs {
		if checkHealth(url, currentBackend) {
			log.Debugf("HealthCheck is up [%s]: Upsert in server list", url.String())
			currentBackend.LB.UpsertServer(url, roundrobin.Weight(1))
		} else {
			log.Warnf("HealthCheck is still failing [%s]", url.String())
			newDisabledURLs = append(newDisabledURLs, url)
		}
	}
	currentBackend.disabledURLs = newDisabledURLs

	for _, url := range enabledURLs {
		if !checkHealth(url, currentBackend) {
			log.Warnf("HealthCheck has failed [%s]: Remove from server list", url.String())
			currentBackend.LB.RemoveServer(url)
			currentBackend.disabledURLs = append(currentBackend.disabledURLs, url)
		}
	}
}

func checkHealth(serverURL *url.URL, backend *BackendHealthCheck) bool {
	client := http.Client{
		Timeout: backend.requestTimeout,
	}
	resp, err := client.Get(serverURL.String() + backend.URL)
	if err == nil {
		defer resp.Body.Close()
	}
	return err == nil && resp.StatusCode == 200
}
