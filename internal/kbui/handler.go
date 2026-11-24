// SPDX-License-Identifier: Apache-2.0

// Package kbui serves a zero-dependency, read-only embedded Go HTML explorer
// for RunLore's OKF knowledge catalog and optional outcome ledger.
//
// Security posture: zero JavaScript, zero external assets, escape-first
// markdown rendering, strict Content-Security-Policy on every response, and
// optional constant-time Bearer token authentication.
package kbui

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/outcome"
)

//go:embed templates/*.html
var templateFS embed.FS

var (
	indexTmpl = template.Must(template.ParseFS(templateFS, "templates/index.html"))
	entryTmpl = template.Must(template.ParseFS(templateFS, "templates/entry.html"))
)

const (
	// ContentSecurityPolicy is the strict zero-JS CSP emitted on every response.
	ContentSecurityPolicy = "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

	defaultStaleAfter = 90 * 24 * time.Hour
	defaultDecayK     = 2.0
)

// Options configures a read-only KB HTML explorer Handler.
type Options struct {
	Catalog    *catalog.Catalog
	Ledger     *outcome.Ledger // optional; nil omits live outcome metrics
	BasePath   string          // mount prefix, defaults to "/kb"
	TokenEnv   string          // optional env var holding a Bearer token
	Now        func() time.Time
	StaleAfter time.Duration // defaults to 90 days
	DecayK     float64       // Beta prior strength for outcome.Aggregate.Factor; defaults to 2.0
}

// Handler serves the read-only KB HTML explorer over HTTP.
type Handler struct {
	cat        *catalog.Catalog
	ledger     *outcome.Ledger
	basePath   string
	tokenEnv   string
	now        func() time.Time
	staleAfter time.Duration
	decayK     float64
}

// New constructs a read-only KB HTML explorer Handler.
func New(opts Options) *Handler {
	basePath := strings.TrimRight(opts.BasePath, "/")
	if basePath == "" {
		basePath = "/kb"
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	staleAfter := opts.StaleAfter
	if staleAfter <= 0 {
		staleAfter = defaultStaleAfter
	}
	decayK := opts.DecayK
	if decayK <= 0 {
		decayK = defaultDecayK
	}
	return &Handler{
		cat:        opts.Catalog,
		ledger:     opts.Ledger,
		basePath:   basePath,
		tokenEnv:   opts.TokenEnv,
		now:        nowFn,
		staleAfter: staleAfter,
		decayK:     decayK,
	}
}

// ServeHTTP enforces security headers, GET/HEAD-only methods, optional Bearer
// authentication, and routes to the index or entry view.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.tokenEnv != "" && !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	p := r.URL.Path
	switch {
	case p == "/" || p == h.basePath || p == h.basePath+"/":
		h.handleIndex(w, r)
	case strings.HasPrefix(p, h.basePath+"/entry/"):
		h.handleEntry(w, r, strings.TrimPrefix(p, h.basePath+"/entry/"))
	case strings.HasPrefix(p, "/entry/"):
		h.handleEntry(w, r, strings.TrimPrefix(p, "/entry/"))
	default:
		http.NotFound(w, r)
	}
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", ContentSecurityPolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func (h *Handler) authorized(r *http.Request) bool {
	want := os.Getenv(h.tokenEnv)
	if want == "" {
		return false
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

type outcomeView struct {
	Present       bool
	Recalls       int
	Resolved      int
	ResolveRate   string
	FeedbackUp    int
	FeedbackDown  int
	Confirms      int
	Verified      int
	Failed        int
	TrustFactor   string
	LastConfirmed string
}

type entryRow struct {
	Path          string
	Title         string
	Type          string
	Status        string
	Origin        string
	Resource      string
	LastValidated string
	Stale         bool
	Outcome       outcomeView
}

type indexData struct {
	BasePath     string
	Query        string
	TypeFilter   string
	StatusFilter string
	OriginFilter string
	HasLedger    bool
	TotalCount   int
	Rows         []entryRow
}

type entryDetailData struct {
	BasePath      string
	Entry         catalog.Entry
	EffectiveStat string
	Origin        string
	LastValidated string
	Stale         bool
	ShortFP       string
	HasLedger     bool
	Outcome       outcomeView
	BodyHTML      template.HTML
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	typeFilter := strings.TrimSpace(r.URL.Query().Get("type"))
	statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))
	originFilter := strings.TrimSpace(r.URL.Query().Get("origin"))

	var entries []catalog.Entry
	if h.cat != nil {
		if q != "" {
			limit := max(h.cat.Len(), 100)
			hits, err := h.cat.SearchScored(q, limit)
			if err != nil {
				http.Error(w, "search failed", http.StatusInternalServerError)
				return
			}
			entries = make([]catalog.Entry, len(hits))
			for i, hit := range hits {
				entries[i] = hit.Entry
			}
		} else {
			entries = h.cat.Entries()
			sort.Slice(entries, func(i, j int) bool {
				if entries[i].Commons != entries[j].Commons {
					return !entries[i].Commons
				}
				return entries[i].Path < entries[j].Path
			})
		}
	}

	var counts map[string]outcome.Aggregate
	hasLedger := h.ledger != nil
	if hasLedger {
		counts, _ = h.ledger.OpenCounts()
	}

	now := h.now()
	rows := make([]entryRow, 0, len(entries))
	for _, e := range entries {
		effStatus := effectiveStatus(e.Status)
		origin := entryOrigin(e.Commons)
		if typeFilter != "" && !strings.EqualFold(e.Type, typeFilter) {
			continue
		}
		if statusFilter != "" && !strings.EqualFold(effStatus, statusFilter) {
			continue
		}
		if originFilter != "" && !strings.EqualFold(origin, originFilter) {
			continue
		}
		lv, stale := h.validationState(e, now)
		rows = append(rows, entryRow{
			Path:          e.Path,
			Title:         e.Title,
			Type:          e.Type,
			Status:        effStatus,
			Origin:        origin,
			Resource:      e.Resource,
			LastValidated: lv,
			Stale:         stale,
			Outcome:       h.buildOutcomeView(counts, e.Path, hasLedger),
		})
	}

	data := indexData{
		BasePath:     h.basePath,
		Query:        q,
		TypeFilter:   typeFilter,
		StatusFilter: statusFilter,
		OriginFilter: originFilter,
		HasLedger:    hasLedger,
		TotalCount:   len(rows),
		Rows:         rows,
	}
	renderTemplate(w, r, indexTmpl, data)
}

func (h *Handler) handleEntry(w http.ResponseWriter, r *http.Request, relPath string) {
	if !isSafeEntryPath(relPath) || h.cat == nil {
		http.NotFound(w, r)
		return
	}

	var found catalog.Entry
	ok := false
	for _, e := range h.cat.Entries() {
		if e.Path == relPath {
			found = e
			ok = true
			break
		}
	}
	if !ok {
		http.NotFound(w, r)
		return
	}

	var counts map[string]outcome.Aggregate
	hasLedger := h.ledger != nil
	if hasLedger {
		counts, _ = h.ledger.OpenCounts()
	}

	lv, stale := h.validationState(found, h.now())
	shortFP := found.Fingerprint
	if len(shortFP) > 12 {
		shortFP = shortFP[:12] + "…"
	}

	data := entryDetailData{
		BasePath:      h.basePath,
		Entry:         found,
		EffectiveStat: effectiveStatus(found.Status),
		Origin:        entryOrigin(found.Commons),
		LastValidated: lv,
		Stale:         stale,
		ShortFP:       shortFP,
		HasLedger:     hasLedger,
		Outcome:       h.buildOutcomeView(counts, found.Path, hasLedger),
		BodyHTML:      RenderMarkdown(found.Body),
	}
	renderTemplate(w, r, entryTmpl, data)
}

func isSafeEntryPath(relPath string) bool {
	if relPath == "" || strings.HasPrefix(relPath, "/") || strings.ContainsAny(relPath, "\\\x00") {
		return false
	}
	for _, seg := range strings.Split(relPath, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return false
		}
	}
	return true
}

func effectiveStatus(s string) string {
	if strings.TrimSpace(s) == "" {
		return "active"
	}
	return strings.TrimSpace(s)
}

func entryOrigin(commons bool) string {
	if commons {
		return "commons"
	}
	return "local"
}

func (h *Handler) validationState(e catalog.Entry, now time.Time) (label string, stale bool) {
	raw := strings.TrimSpace(e.LastValidated)
	if raw == "" {
		raw = strings.TrimSpace(e.Timestamp)
	}
	if raw == "" {
		return "-", false
	}
	t, ok := catalog.ParseEntryDate(raw)
	if !ok {
		return raw, false
	}
	return t.UTC().Format("2006-01-02"), now.Sub(t) > h.staleAfter
}

func (h *Handler) buildOutcomeView(counts map[string]outcome.Aggregate, path string, hasLedger bool) outcomeView {
	if !hasLedger {
		return outcomeView{}
	}
	agg := counts[path]
	v := outcomeView{
		Present:      true,
		Recalls:      agg.Recalls,
		Resolved:     agg.Resolved,
		ResolveRate:  "-",
		FeedbackUp:   agg.FeedbackUp,
		FeedbackDown: agg.FeedbackDown,
		Confirms:     agg.Confirms,
		Verified:     agg.Verified,
		Failed:       agg.Failed,
		TrustFactor:  "-",
	}
	if agg.Recalls > 0 {
		v.ResolveRate = fmt.Sprintf("%d/%d", agg.Resolved, agg.Recalls)
	}
	if agg.Recalls > 0 || agg.HasEvidence() {
		v.TrustFactor = fmt.Sprintf("%.2f", agg.Factor(h.decayK))
	}
	if !agg.LastConfirmed.IsZero() {
		v.LastConfirmed = agg.LastConfirmed.UTC().Format("2006-01-02")
	}
	return v
}

func renderTemplate(w http.ResponseWriter, r *http.Request, tmpl *template.Template, data any) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(buf.Bytes())
}
