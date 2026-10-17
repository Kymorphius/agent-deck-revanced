package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/core/daemon"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

// Client is concurrent-safe. Close releases only this client's connections;
// matrix/hardened: it never shuts down the collector or an AI session.
type Client struct {
	socket    string
	ensure    func(context.Context) error
	ensureMu  sync.Mutex
	http      *http.Client
	transport *http.Transport
	closed    atomic.Bool
	ctx       context.Context
	cancel    context.CancelFunc
}

// IsAbsent is the only condition that may invoke lazy process creation.
func IsAbsent(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func NewClient(socket string, ensure func(context.Context) error) *Client {
	c := &Client{socket: socket, ensure: ensure}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.transport = &http.Transport{Proxy: nil, MaxIdleConns: 4, MaxIdleConnsPerHost: 4, MaxConnsPerHost: 8, IdleConnTimeout: 20 * time.Second, ResponseHeaderTimeout: 12 * time.Second, DisableCompression: true}
	c.transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		if c.closed.Load() {
			return nil, ErrClosed
		}
		if err := ValidatePaths(daemon.PathsIn(filepath.Dir(socket))); err != nil {
			return nil, err
		}
		dialer := net.Dialer{Timeout: 2 * time.Second}
		conn, err := dialer.DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, err
		}
		if err = checkPeer(conn, os.Getuid()); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
	c.http = &http.Client{Transport: c.transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c
}

func (c *Client) Close() error {
	c.closed.Store(true)
	c.cancel()
	c.transport.CloseIdleConnections()
	return nil
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, out any, start bool) error {
	if c.closed.Load() {
		return ErrClosed
	}
	err := c.requestOnce(ctx, method, path, query, out)
	if err == nil || !start || c.ensure == nil || ctx.Err() != nil || c.closed.Load() {
		return err
	}
	// Only a refused/absent transport may start an owner. Protocol/permission/
	// source errors must never create a competing collector or retry mutations.
	if !IsAbsent(err) {
		return err
	}
	c.ensureMu.Lock()
	defer c.ensureMu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}
	if retry := c.requestOnce(ctx, method, path, query, out); retry == nil {
		return nil
	} else if !IsAbsent(retry) {
		return retry
	}
	if ensureErr := c.ensure(ctx); ensureErr != nil {
		return fmt.Errorf("start runtime monitor: %w", ensureErr)
	}
	return c.requestOnce(ctx, method, path, query, out)
}
func (c *Client) requestOnce(ctx context.Context, method, path string, query url.Values, out any) error {
	requestCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	address := "http://monitor" + path
	if len(query) > 0 {
		address += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(requestCtx, method, address, nil)
	if err != nil {
		return err
	}
	req.Header.Set(protocolHeader, strconv.Itoa(Protocol))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.Header.Get(protocolHeader) != strconv.Itoa(Protocol) {
		return fmt.Errorf("monitor response protocol mismatch")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("monitor response exceeds limit")
	}
	if resp.StatusCode != http.StatusOK {
		var failure wireError
		if json.Unmarshal(data, &failure) != nil {
			return fmt.Errorf("monitor returned HTTP %d", resp.StatusCode)
		}
		if failure.Gap != nil {
			return failure.Gap
		}
		return fmt.Errorf("monitor: %s", failure.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) Snapshot(ctx context.Context, profile string) (*session.RuntimeProfileSnapshot, error) {
	var snapshot session.RuntimeProfileSnapshot
	err := c.request(ctx, http.MethodGet, "/snapshot", url.Values{"profile": {profile}}, &snapshot, true)
	if err == nil {
		err = validateSnapshot(snapshot, profile)
	}
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}
func (c *Client) Refresh(ctx context.Context, profile string) error {
	var snapshot session.RuntimeProfileSnapshot
	err := c.request(ctx, http.MethodPost, "/refresh", url.Values{"profile": {profile}}, &snapshot, true)
	if err == nil {
		err = validateSnapshot(snapshot, profile)
	}
	if err == nil && snapshot.Error != "" {
		err = fmt.Errorf("monitor refresh: %s", snapshot.Error)
	}
	return err
}
func (c *Client) Observations(ctx context.Context, profile, epoch string, after uint64) ([]session.RuntimeProfileSnapshot, error) {
	var snapshots []session.RuntimeProfileSnapshot
	err := c.request(ctx, http.MethodGet, "/observations", url.Values{"profile": {profile}, "epoch": {epoch}, "after": {strconv.FormatUint(after, 10)}}, &snapshots, true)
	if err != nil {
		return nil, err
	}
	last := after
	for _, snapshot := range snapshots {
		if err = validateSnapshot(snapshot, profile); err != nil {
			return nil, err
		}
		if (epoch != "" && snapshot.Epoch != epoch) || snapshot.Revision <= last {
			return nil, fmt.Errorf("monitor returned unordered observations")
		}
		last = snapshot.Revision
	}
	return snapshots, nil
}
func validateSnapshot(snapshot session.RuntimeProfileSnapshot, profile string) error {
	if snapshot.Protocol != Protocol || snapshot.Profile != profile || snapshot.Epoch == "" || snapshot.Revision == 0 {
		return fmt.Errorf("invalid runtime monitor snapshot identity")
	}
	return nil
}
func (c *Client) Status(ctx context.Context) (Status, error) {
	var status Status
	err := c.request(ctx, http.MethodGet, "/status", nil, &status, false)
	return status, err
}
func (c *Client) Stop(ctx context.Context) error {
	return c.request(ctx, http.MethodPost, "/stop", nil, nil, false)
}

var _ session.RuntimeObservationSource = (*Client)(nil)
