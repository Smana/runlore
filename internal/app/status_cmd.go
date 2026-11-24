// SPDX-License-Identifier: Apache-2.0

package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/providers/gitops/argocd"
	"github.com/Smana/runlore/internal/providers/gitops/flux"
)

const (
	statusWarningWindow    = 15 * time.Minute
	statusMaxWarningEvents = 20
	statusMaxSuggestions   = 5
	statusRestartThreshold = 3
)

// ErrStatusUnhealthy is returned by RunStatus when --exit-code is set and the
// cluster has at least one failing GitOps object or unhealthy pod.
var ErrStatusUnhealthy = errors.New("cluster has unhealthy workloads or failing GitOps objects")

// StatusReport is the structured zero-LLM triage snapshot emitted by `lore status`.
type StatusReport struct {
	Namespace      string         `json:"namespace,omitempty"`
	GitOpsFailures []StatusGitOps `json:"gitops_failures"`
	UnhealthyPods  []StatusPod    `json:"unhealthy_pods"`
	WarningEvents  []StatusEvent  `json:"warning_events"`
	Suggestions    []string       `json:"suggestions"`
}

// UnhealthyCount returns the number of failing GitOps objects plus unhealthy pods.
func (r StatusReport) UnhealthyCount() int {
	return len(r.GitOpsFailures) + len(r.UnhealthyPods)
}

// StatusGitOps describes one failing Flux or Argo CD object.
type StatusGitOps struct {
	Engine    string    `json:"engine"`
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message,omitempty"`
	When      time.Time `json:"when,omitempty"`
}

// StatusPod describes one unhealthy Kubernetes pod.
type StatusPod struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Reason    string `json:"reason"`
	Restarts  int32  `json:"restarts"`
	Message   string `json:"message,omitempty"`
}

// StatusEvent describes one recent Kubernetes Warning event.
type StatusEvent struct {
	Namespace    string    `json:"namespace"`
	InvolvedKind string    `json:"involved_kind"`
	InvolvedName string    `json:"involved_name"`
	Reason       string    `json:"reason"`
	Message      string    `json:"message"`
	Count        int32     `json:"count,omitempty"`
	LastSeen     time.Time `json:"last_seen"`
}

// RunStatus runs a zero-LLM read-only triage snapshot against the active cluster
// (or --kube-context) and prints unhealthy pods, failing GitOps objects, recent
// Warning events, and ready-to-run `lore investigate` commands.
func RunStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	namespace := fs.String("namespace", "", "scope snapshot to one namespace (default: all namespaces)")
	kubeContext := fs.String("kube-context", "", "kubeconfig context to use")
	jsonOut := fs.Bool("json", false, "emit status snapshot as JSON on stdout")
	exitCode := fs.Bool("exit-code", false, "exit non-zero if any GitOps failures or unhealthy pods are found")

	restArgs, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(restArgs) > 0 {
		return fmt.Errorf("status takes no positional arguments, got %q", strings.Join(restArgs, " "))
	}

	restCfg, err := RestConfigForContext(*kubeContext)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build kubernetes client: %w", err)
	}
	var listers []providers.GitOpsFailureLister
	if dc, derr := dynamic.NewForConfig(restCfg); derr == nil {
		listers = append(listers,
			flux.New(flux.NewDynamicReader(dc), nil),
			argocd.New(argocd.NewDynamicReader(dc), nil),
		)
	}

	report, err := collectStatus(context.Background(), cs, listers, *namespace, time.Now())
	if err != nil {
		return err
	}
	if err := renderStatus(os.Stdout, report, *jsonOut); err != nil {
		return err
	}
	return statusExitError(report, *exitCode)
}

func statusExitError(report StatusReport, exitCode bool) error {
	if exitCode && report.UnhealthyCount() > 0 {
		return ErrStatusUnhealthy
	}
	return nil
}

func collectStatus(ctx context.Context, cs kubernetes.Interface, listers []providers.GitOpsFailureLister, namespace string, now time.Time) (StatusReport, error) {
	report := StatusReport{
		Namespace:      namespace,
		GitOpsFailures: []StatusGitOps{},
		UnhealthyPods:  []StatusPod{},
		WarningEvents:  []StatusEvent{},
		Suggestions:    []string{},
	}

	for _, lister := range listers {
		if lister == nil {
			continue
		}
		failures, err := lister.ListFailures(ctx, namespace)
		if err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
				continue
			}
			return StatusReport{}, fmt.Errorf("list gitops failures: %w", err)
		}
		for _, f := range failures {
			report.GitOpsFailures = append(report.GitOpsFailures, StatusGitOps{
				Engine:    string(f.Engine),
				Kind:      f.Workload.Kind,
				Namespace: f.Workload.Namespace,
				Name:      f.Workload.Name,
				Reason:    f.Reason,
				Message:   f.Message,
				When:      f.When,
			})
		}
	}
	slices.SortFunc(report.GitOpsFailures, func(a, b StatusGitOps) int {
		return cmp.Or(
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Name, b.Name),
		)
	})

	listNS := namespace
	if listNS == "" {
		listNS = metav1.NamespaceAll
	}
	pods, err := cs.CoreV1().Pods(listNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		return StatusReport{}, fmt.Errorf("list pods: %w", err)
	}
	for i := range pods.Items {
		if sp, unhealthy := classifyPod(&pods.Items[i]); unhealthy {
			report.UnhealthyPods = append(report.UnhealthyPods, sp)
		}
	}
	slices.SortFunc(report.UnhealthyPods, func(a, b StatusPod) int {
		return cmp.Or(
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Name, b.Name),
		)
	})

	events, err := cs.CoreV1().Events(listNS).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
	if err != nil {
		return StatusReport{}, fmt.Errorf("list warning events: %w", err)
	}
	cutoff := now.Add(-statusWarningWindow)
	for i := range events.Items {
		ev := &events.Items[i]
		if ev.Type != corev1.EventTypeWarning {
			continue
		}
		ts := eventTimestamp(ev)
		if ts.IsZero() || ts.Before(cutoff) {
			continue
		}
		report.WarningEvents = append(report.WarningEvents, StatusEvent{
			Namespace:    ev.Namespace,
			InvolvedKind: ev.InvolvedObject.Kind,
			InvolvedName: ev.InvolvedObject.Name,
			Reason:       ev.Reason,
			Message:      ev.Message,
			Count:        ev.Count,
			LastSeen:     ts,
		})
	}
	slices.SortFunc(report.WarningEvents, func(a, b StatusEvent) int {
		if b.LastSeen.After(a.LastSeen) {
			return 1
		}
		if a.LastSeen.After(b.LastSeen) {
			return -1
		}
		return cmp.Or(
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.InvolvedName, b.InvolvedName),
		)
	})
	if len(report.WarningEvents) > statusMaxWarningEvents {
		report.WarningEvents = report.WarningEvents[:statusMaxWarningEvents]
	}

	report.Suggestions = buildStatusSuggestions(report)
	return report, nil
}

func classifyPod(p *corev1.Pod) (StatusPod, bool) {
	var maxRestarts int32
	var waitReason, waitMsg string
	for _, cs := range p.Status.InitContainerStatuses {
		if cs.RestartCount > maxRestarts {
			maxRestarts = cs.RestartCount
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && waitReason == "" {
			waitReason = cs.State.Waiting.Reason
			waitMsg = cs.State.Waiting.Message
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.RestartCount > maxRestarts {
			maxRestarts = cs.RestartCount
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && waitReason == "" {
			waitReason = cs.State.Waiting.Reason
			waitMsg = cs.State.Waiting.Message
		}
	}

	sp := StatusPod{
		Namespace: p.Namespace,
		Name:      p.Name,
		Phase:     string(p.Status.Phase),
		Restarts:  maxRestarts,
		Message:   waitMsg,
	}

	switch {
	case waitReason != "":
		sp.Reason = waitReason
		return sp, true
	case p.Status.Phase == corev1.PodFailed:
		sp.Reason = cmp.Or(p.Status.Reason, "Failed")
		sp.Message = cmp.Or(sp.Message, p.Status.Message)
		return sp, true
	case p.Status.Phase == corev1.PodPending:
		sp.Reason = cmp.Or(p.Status.Reason, "Pending")
		sp.Message = cmp.Or(sp.Message, p.Status.Message)
		return sp, true
	case maxRestarts >= statusRestartThreshold:
		sp.Reason = "HighRestarts"
		return sp, true
	case p.Status.Phase == corev1.PodRunning && !podReady(p):
		sp.Reason = "NotReady"
		return sp, true
	default:
		return StatusPod{}, false
	}
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func eventTimestamp(ev *corev1.Event) time.Time {
	if !ev.LastTimestamp.IsZero() {
		return ev.LastTimestamp.Time
	}
	if !ev.EventTime.IsZero() {
		return ev.EventTime.Time
	}
	return ev.CreationTimestamp.Time
}

func buildStatusSuggestions(r StatusReport) []string {
	out := make([]string, 0, statusMaxSuggestions)
	seen := map[string]bool{}
	add := func(cmd string) {
		if len(out) >= statusMaxSuggestions || seen[cmd] {
			return
		}
		seen[cmd] = true
		out = append(out, cmd)
	}
	for _, g := range r.GitOpsFailures {
		add(fmt.Sprintf("lore investigate %s/%s/%s", g.Kind, g.Namespace, g.Name))
	}
	for _, p := range r.UnhealthyPods {
		add(fmt.Sprintf("lore investigate Pod/%s/%s", p.Namespace, p.Name))
	}
	return out
}

func renderStatus(w io.Writer, r StatusReport, jsonOut bool) error {
	if jsonOut {
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return fmt.Errorf("encode status JSON: %w", err)
		}
		_, err = fmt.Fprintf(w, "%s\n", b)
		return err
	}
	if len(r.GitOpsFailures) == 0 && len(r.UnhealthyPods) == 0 && len(r.WarningEvents) == 0 {
		_, err := fmt.Fprintln(w, "Cluster healthy (0 GitOps failures, 0 unhealthy pods, 0 recent warning events).")
		return err
	}

	var werr error
	writef := func(dst io.Writer, format string, args ...any) {
		if werr != nil {
			return
		}
		_, werr = fmt.Fprintf(dst, format, args...)
	}
	writeln := func(dst io.Writer, args ...any) {
		if werr != nil {
			return
		}
		_, werr = fmt.Fprintln(dst, args...)
	}

	section := 0
	sep := func() {
		if section > 0 {
			writeln(w)
		}
		section++
	}

	if len(r.GitOpsFailures) > 0 {
		sep()
		writef(w, "GitOps failures (%d)\n", len(r.GitOpsFailures))
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		writeln(tw, "  TARGET\tREASON\tMESSAGE")
		for _, g := range r.GitOpsFailures {
			writef(tw, "  %s/%s/%s\t%s\t%s\n", g.Kind, g.Namespace, g.Name, g.Reason, singleLineTrim(g.Message, 80))
		}
		if err := tw.Flush(); err != nil && werr == nil {
			werr = err
		}
	}

	if len(r.UnhealthyPods) > 0 {
		sep()
		writef(w, "Unhealthy pods (%d)\n", len(r.UnhealthyPods))
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		writeln(tw, "  POD\tPHASE\tREASON\tRESTARTS")
		for _, p := range r.UnhealthyPods {
			writef(tw, "  %s/%s\t%s\t%s\t%d\n", p.Namespace, p.Name, p.Phase, p.Reason, p.Restarts)
		}
		if err := tw.Flush(); err != nil && werr == nil {
			werr = err
		}
	}

	if len(r.WarningEvents) > 0 {
		sep()
		writef(w, "Recent warning events (%d, last 15m)\n", len(r.WarningEvents))
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		writeln(tw, "  OBJECT\tREASON\tCOUNT\tMESSAGE")
		for _, e := range r.WarningEvents {
			obj := fmt.Sprintf("%s/%s/%s", e.InvolvedKind, e.Namespace, e.InvolvedName)
			count := max(e.Count, 1)
			writef(tw, "  %s\t%s\tx%d\t%s\n", obj, e.Reason, count, singleLineTrim(e.Message, 80))
		}
		if err := tw.Flush(); err != nil && werr == nil {
			werr = err
		}
	}

	if len(r.Suggestions) > 0 {
		sep()
		writeln(w, "Next step:")
		for _, cmd := range r.Suggestions {
			writef(w, "  %s\n", cmd)
		}
	}
	return werr
}

func singleLineTrim(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}
