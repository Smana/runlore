// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"log/slog"
)

// Replier posts a reply into an existing thread. Implemented by the chat
// notifiers that can carry a conversation back (see providers.ThreadNotifier).
type Replier interface {
	ReplyInThread(ctx context.Context, root, channel, text string) error
}

// Mention is the transport-facing entry point: it resolves the thread, runs the
// responder, and posts the answer. It exists so a transport handler stays a
// parser — everything downstream of "a human addressed us in thread X" is here.
type Mention struct {
	Responder *Responder
	Registry  *Registry
	Replier   Replier
	Log       *slog.Logger
}

// HandleMention processes one addressed message. It never returns an error: it
// runs detached from the request that delivered it, so every failure is a log
// line plus, wherever possible, a reply the human can see.
func (m *Mention) HandleMention(ctx context.Context, channel, root, author, text string) {
	tc, ok := m.Registry.Get(root)
	if !ok {
		m.Log.Info("thread: mention in an unrecognised thread", "root", root, "channel", channel, "author", author)
		m.reply(ctx, root, channel,
			"I don't have context for this thread — I can only record knowledge in a thread I started, "+
				"and only for a limited time after the finding was posted.")
		return
	}
	// The channel is taken from the live event rather than the stored context: a
	// message can only be replied to where it was actually sent.
	tc.Channel = channel

	reply, err := m.Responder.Handle(ctx, tc, author, text)
	if err != nil {
		m.Log.Warn("thread: knowledge write failed", "root", root, "author", author, "err", err)
	}
	m.reply(ctx, root, channel, reply)
}

// Busy tells the human their message could not be accepted right now, so they know
// to send it again rather than assume it was recorded. It is called off the request
// goroutine that received the mention, under its own small concurrency budget — see
// Server.handleSlackEvent — so it can never itself become an unbounded liability.
// Best-effort and nil-Replier-safe, exactly like reply.
func (m *Mention) Busy(ctx context.Context, channel, root string) {
	m.reply(ctx, root, channel,
		"I'm handling too many messages right now — please send that again in a moment.")
}

// reply posts best-effort. The knowledge write has already succeeded by this
// point and is never rolled back because the acknowledgement could not be
// delivered.
func (m *Mention) reply(ctx context.Context, root, channel, text string) {
	if m.Replier == nil || text == "" {
		return
	}
	if err := m.Replier.ReplyInThread(ctx, root, channel, text); err != nil {
		m.Log.Warn("thread: reply failed (best-effort)", "root", root, "err", err)
	}
}
