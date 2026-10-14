package ui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// A worker owns its channel, never the model. Each delivered snapshot schedules
// one more receiver, so Update can paint cached data before networking finishes.
type streamMsg struct {
	message tea.Msg
	next    tea.Cmd
}

func streamCommand(ctx context.Context, cancel context.CancelFunc, run func(func(tea.Msg)) error, done func(error) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		if ctx.Err() != nil {
			return nil
		}
		messages := make(chan tea.Msg, 1)
		go func() {
			defer close(messages)
			emit := func(msg tea.Msg) {
				select {
				case messages <- msg:
				case <-ctx.Done():
				}
			}
			err := run(emit)
			emit(done(err))
		}()
		return receiveStream(ctx, cancel, messages)()
	}
}

func receiveStream(ctx context.Context, cancel context.CancelFunc, messages <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-messages:
			if !ok {
				cancel()
				return nil
			}
			return streamMsg{message: msg, next: receiveStream(ctx, cancel, messages)}
		}
	}
}
