// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"errors"
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
//
// fallback is an optional Context a transport may have decoded some other way
// than the registry — off the delivered event itself, for a transport that
// stamps one there — for use ONLY when the registry misses on root (TTL
// expiry, restart, leader failover). A registry hit always wins over a
// supplied fallback: the registry carries NoteURL/Notes state a fresh stamp
// cannot reconstruct. nil when the caller has no such source; Slack's own
// transport never does, since nothing about a Slack event carries the
// investigation context.
//
// A fallback that IS used is also persisted under root, so the registry
// "learns" the thread: every later message hits normally, and the per-thread
// cap, the NoteURL write-back and the note counter all keep working from then
// on, instead of the fallback being substituted fresh (and un-countable) on
// every single reply.
//
// The registry miss and the fallback rehydration are done as one atomic
// Registry.GetOrCreate call rather than a separate Get() followed by a Put():
// two messages arriving close together in a never-before-tracked thread would
// otherwise both miss Get, both build their own copy of the fallback, and
// both Put it — each Put is itself atomic, but the pair is not, so a late Put
// could CLOBBER an earlier caller's entry, discarding NoteURL/Notes state
// that caller had already written back after its own forge write landed.
// GetOrCreate closes that: every caller shares the one entry the first
// caller's goroutine persists, instead of racing to create (and potentially
// clobber) their own.
//
// GetOrCreate's atomicity alone does not stop two callers who both observe
// that ONE shared entry's NoteURL == "" from both taking the OpenPR route
// below — what closes that is Responder.write's per-root guard, which
// serialises writers for a root and re-reads the registry before choosing a
// route, so the second writer sees the first writer's freshly-landed
// NoteURL. See Registry.GetOrCreate's doc comment for the full account of
// which mechanism closes which outcome.
func (m *Mention) HandleMention(ctx context.Context, channel, root, author, text string, fallback *Context) {
	tc, ok := m.Registry.Get(root)
	if !ok {
		if fallback == nil {
			m.Log.Info("thread: mention in an unrecognised thread", "root", root, "channel", channel, "author", author)
			m.reply(ctx, root, channel,
				"I don't have context for this thread — I can only record knowledge in a thread I started, "+
					"and only for a limited time after the finding was posted.")
			return
		}
		var created bool
		var err error
		tc, created, err = m.Registry.GetOrCreate(root, *fallback)
		switch {
		case errors.Is(err, ErrThreadNotEstablishable):
			// The registry could not record anything at all (disabled, or an
			// empty root) — NOT the same outcome as a concurrent caller already
			// winning the race (created=false, err=nil): that case hands back a
			// real, persisted entry to use. This one hands back the zero-value
			// Context, which must never be mistaken for "the established entry"
			// and carried into a write — that is exactly what would open a
			// contextless "Operator note" PR with no title, trigger key, or
			// resource, for every message, bounded only by the global hourly
			// window. Refuse the same way an outright registry miss with no
			// fallback does.
			m.Log.Info("thread: mention in an unrecognised thread and the registry cannot establish one",
				"root", root, "channel", channel, "author", author, "err", err)
			m.reply(ctx, root, channel,
				"I don't have context for this thread — I can only record knowledge in a thread I started, "+
					"and only for a limited time after the finding was posted.")
			return
		case err != nil:
			// The rehydration write itself failed (e.g. a disk error): the fallback
			// is still used for THIS message — refusing outright would drop the
			// human's words for certain — but the cap and counter may not be
			// enforced for this thread until a later write succeeds.
			m.Log.Warn("thread: could not rehydrate the registry from a fallback context; this thread's cap may not be enforced",
				"root", root, "channel", channel, "author", author, "err", err)
		case !created:
			// Another goroutine's concurrent message won the race to rehydrate this
			// thread first; this one observed and is using that entry instead.
			m.Log.Info("thread: registry already rehydrated by a concurrent message; using the established entry",
				"root", root, "channel", channel, "author", author)
		}
	}
	// The channel is taken from the live event rather than the stored (or
	// just-rehydrated) context: a message can only be replied to where it was
	// actually sent.
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
