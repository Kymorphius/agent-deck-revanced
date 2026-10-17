package nativeui

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: validate resize observation without a real desktop or timer.
func TestWindowSizeObserverDeduplicatesAndRejectsInvalidSizes(t *testing.T) {
	type sample struct {
		width, height int
		err           error
	}
	samples := []sample{
		{80, 24, nil},
		{80, 24, nil},
		{120, 30, errors.New("console temporarily unavailable")},
		{0, 24, nil},
		{-1, 24, nil},
		{80, 0, nil},
		{80, -1, nil},
		{80, 24, nil},
		{120, 30, nil},
		{120, 30, nil},
		{0, 0, errors.New("invalid handle")},
		{80, 24, nil},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	read := make(chan struct{}, len(samples))
	done := make(chan struct{})
	var sent []tea.WindowSizeMsg
	go func() {
		defer close(done)
		index := 0
		observeWindowSizes(ctx, ticks, func() (int, int, error) {
			current := samples[index]
			index++
			read <- struct{}{}
			return current.width, current.height, current.err
		}, func(msg tea.Msg) {
			sent = append(sent, msg.(tea.WindowSizeMsg))
		})
	}()
	for index := range samples {
		if index > 0 {
			select {
			case ticks <- time.Time{}:
			case <-time.After(3 * time.Second):
				t.Fatal("observer stopped before reading all sizes")
			}
		}
		select {
		case <-read:
		case <-time.After(3 * time.Second):
			t.Fatal("observer did not read console size")
		}
	}
	close(ticks)
	waitResizeObserver(t, done)
	want := []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 120, Height: 30}, {Width: 80, Height: 24}}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("geometry changes = %#v, want %#v", sent, want)
	}
}

func TestWindowSizeObserverCancelledBeforeStartDoesNotRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	observeWindowSizes(ctx, nil, func() (int, int, error) {
		t.Fatal("cancelled observer read console geometry")
		return 80, 24, nil
	}, func(tea.Msg) { t.Fatal("cancelled observer sent a message") })
}

func TestWindowSizeObserverCancellationStopsIdlePoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := make(chan tea.Msg, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		observeWindowSizes(ctx, nil, func() (int, int, error) { return 80, 24, nil }, func(msg tea.Msg) { sent <- msg })
	}()
	select {
	case <-sent:
	case <-time.After(3 * time.Second):
		t.Fatal("observer did not report its initial geometry")
	}
	cancel()
	waitResizeObserver(t, done)
}

// matrix/hardened: Exec pauses Tea's receive loop in the same way as an
// unstarted program. A shared WithContext must release its blocked Send.
func TestWindowSizeObserverCancellationReleasesBlockedProgramSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	program := tea.NewProgram(New(nil, "."), tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler())
	sending := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		observeWindowSizes(ctx, nil, func() (int, int, error) { return 80, 24, nil }, func(msg tea.Msg) {
			close(sending)
			program.Send(msg)
		})
	}()
	select {
	case <-sending:
	case <-time.After(3 * time.Second):
		t.Fatal("observer did not attempt Program.Send")
	}
	select {
	case <-done:
		t.Fatal("Program.Send unexpectedly returned before cancellation")
	default:
	}
	cancel()
	waitResizeObserver(t, done)
}

func waitResizeObserver(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("resize observer did not shut down")
	}
}
