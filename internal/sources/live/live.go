// Package live talks to a Kubernetes cluster via client-go and
// produces finding.Object records for every deprecated GVK currently
// served by the apiserver, plus any "shadow APIs" stored in Helm v3
// release secrets that the apiserver may no longer serve.
package live

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/sources/manifests"
)

// Client bundles the client variants we need.
type Client struct {
	Discovery discovery.DiscoveryInterface
	Dyn       dynamic.Interface
	Core      kubernetes.Interface
	cfg       *rest.Config
}

// Connect returns a Client. kubeconfigPath = "" uses the default
// loading rules (KUBECONFIG env, ~/.kube/config). contextName = ""
// uses the current-context.
func Connect(kubeconfigPath, contextName string) (*Client, error) {
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loader.ExplicitPath = kubeconfigPath
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loader,
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	cfg.QPS = 50
	cfg.Burst = 100
	cfg.Timeout = 30 * time.Second

	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{Discovery: disco, Dyn: dyn, Core: core, cfg: cfg}, nil
}

// ServerVersion returns the apiserver's reported semver string.
func (c *Client) ServerVersion() (string, error) {
	v, err := c.Discovery.ServerVersion()
	if err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

// listableGVRs returns every (group, version, resource) the apiserver
// advertises that supports the "list" verb.
//
// IMPORTANT: when the apiserver serves the SAME Kind under both an
// old and a new apiVersion (e.g. flowcontrol.apiserver.k8s.io/v1beta3
// and /v1 in K8s 1.30), both list calls return the SAME etcd objects.
// Listing both produces phantom duplicate findings against built-in
// system resources that the apiserver re-bootstraps automatically on
// upgrade. We collapse this by keeping ONLY the most recent
// (highest sort-order) GroupVersion per Kind+Group.
func (c *Client) listableGVRs() ([]schema.GroupVersionResource, []string, error) {
	_, lists, err := c.Discovery.ServerGroupsAndResources()
	if err != nil && !discovery.IsGroupDiscoveryFailedError(err) {
		return nil, nil, err
	}

	// First pass: bucket every (group, kind) → list of (version, resource) candidates.
	buckets := map[string][]gvkCandidate{} // key = "group|kind"
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			if !verbContains(r.Verbs, "list") {
				continue
			}
			if strings.Contains(r.Name, "/") {
				continue
			}
			key := gv.Group + "|" + r.Kind
			buckets[key] = append(buckets[key], gvkCandidate{Version: gv.Version, Resource: r.Name, Kind: r.Kind})
		}
	}

	// Second pass: per bucket, keep the version that sorts LATEST (i.e.
	// non-beta beats beta beats alpha; v2 beats v1; etc.). The K8s
	// version-sort algorithm matches the apiserver's storage version
	// preference under sigs.k8s.io/apimachinery semantics.
	var (
		out   []schema.GroupVersionResource
		kinds []string
	)
	for key, cands := range buckets {
		group := strings.SplitN(key, "|", 2)[0]
		// Sort by Kubernetes version preference (latest first).
		sortByKubeVersion(cands)
		best := cands[0]
		out = append(out, schema.GroupVersionResource{Group: group, Version: best.Version, Resource: best.Resource})
		kinds = append(kinds, best.Kind)
	}
	return out, kinds, nil
}

// gvkCandidate is one (version, resource) entry under a Kind.
type gvkCandidate struct {
	Version  string
	Resource string
	Kind     string
}

// sortByKubeVersion mutates `cands` so that the most-preferred
// version (production > beta > alpha; higher number > lower) is at
// index 0. Implements the same ordering as
// k8s.io/apimachinery/pkg/version.CompareKubeAwareVersionStrings.
func sortByKubeVersion(cands []gvkCandidate) {
	rank := func(v string) (int, int, int) {
		// Returns (majorRank, prereleaseRank, prereleaseNumber).
		// majorRank: integer N from "vN..." (so v2 > v1).
		// prereleaseRank: 3=GA, 2=beta, 1=alpha, 0=other.
		// prereleaseNumber: integer after "beta"/"alpha".
		s := strings.TrimPrefix(v, "v")
		majorPart := s
		stage := 3 // GA default
		stageNum := 0
		for i, marker := range []string{"beta", "alpha"} {
			if idx := strings.Index(s, marker); idx >= 0 {
				majorPart = s[:idx]
				if n, err := strconv.Atoi(s[idx+len(marker):]); err == nil {
					stageNum = n
				}
				stage = 2 - i // beta=2, alpha=1
				break
			}
		}
		major, _ := strconv.Atoi(majorPart)
		return major, stage, stageNum
	}
	for i := 1; i < len(cands); i++ {
		for j := i; j > 0; j-- {
			a, b := cands[j-1], cands[j]
			am, as, an := rank(a.Version)
			bm, bs, bn := rank(b.Version)
			less := false
			switch {
			case am != bm:
				less = bm > am
			case as != bs:
				less = bs > as
			default:
				less = bn > an
			}
			if less {
				cands[j-1], cands[j] = b, a
			} else {
				break
			}
		}
	}
}

func verbContains(verbs []string, want string) bool {
	for _, v := range verbs {
		if v == want {
			return true
		}
	}
	return false
}

// Filter is a predicate over (apiVersion, kind). Live walking is
// expensive — callers should pass a filter that returns true only
// for GVKs they care about.
type Filter func(apiVersion, kind string) bool

// Walk lists every object for every listable GVR that passes the
// filter. Per-GVR errors do not abort the walk.
func (c *Client) Walk(ctx context.Context, filter Filter) ([]manifests.Object, []error) {
	var (
		out  []manifests.Object
		errs []error
	)
	gvrs, kinds, err := c.listableGVRs()
	if err != nil {
		errs = append(errs, fmt.Errorf("discovery: %w", err))
	}
	for i, gvr := range gvrs {
		kind := kinds[i]
		apiVersion := apiVersionFor(gvr)
		if filter != nil && !filter(apiVersion, kind) {
			continue
		}
		list, err := c.Dyn.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || apierrors.IsMethodNotSupported(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("list %s: %w", gvr.String(), err))
			continue
		}
		for _, item := range list.Items {
			out = append(out, manifests.Object{
				Obj: finding.Object{
					APIVersion: apiVersion,
					Kind:       kind,
					Namespace:  item.GetNamespace(),
					Name:       item.GetName(),
				},
				Source: finding.Source{Kind: "live", Location: fmt.Sprintf("%s/%s", apiVersion, gvr.Resource)},
			})
		}
	}
	return out, errs
}

func apiVersionFor(gvr schema.GroupVersionResource) string {
	if gvr.Group == "" {
		return gvr.Version
	}
	return gvr.Group + "/" + gvr.Version
}

// HelmReleases reads Helm v3 release secrets across all namespaces and
// returns each release's rendered manifest split into objects.
//
// This is the shadow-API channel: objects baked into a release whose
// API the apiserver may no longer serve. Helm v3 release secrets are
// type "helm.sh/release.v1" with an "owner=helm" label and a "release"
// data key holding base64(gzip(json(release))).
func (c *Client) HelmReleases(ctx context.Context) ([]manifests.Object, []error) {
	var (
		out  []manifests.Object
		errs []error
	)
	secrets, err := c.Core.CoreV1().Secrets("").List(ctx, metav1.ListOptions{
		LabelSelector: "owner=helm",
	})
	if err != nil {
		return nil, []error{fmt.Errorf("list helm secrets: %w", err)}
	}
	for _, s := range secrets.Items {
		if !isHelmReleaseSecret(s) {
			continue
		}
		release, err := decodeHelmRelease(s.Data["release"])
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s decode: %w", s.Namespace, s.Name, err))
			continue
		}
		loc := fmt.Sprintf("helm:%s/%s", s.Namespace, releaseName(s, release))
		objs, err := manifests.ParseString(release.Manifest, loc)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s parse: %w", loc, err))
			continue
		}
		// Override Source.Kind so renderers can group by helm-release.
		for i := range objs {
			objs[i].Source.Kind = "helm-release"
		}
		out = append(out, objs...)
	}
	return out, errs
}

func isHelmReleaseSecret(s corev1.Secret) bool {
	return s.Type == "helm.sh/release.v1" && len(s.Data["release"]) > 0
}

func releaseName(s corev1.Secret, r *helmRelease) string {
	if r != nil && r.Name != "" {
		return r.Name
	}
	if v, ok := s.Labels["name"]; ok {
		return v
	}
	return s.Name
}

// helmRelease is a tiny shape — we only need the manifest field.
type helmRelease struct {
	Name     string `json:"name"`
	Manifest string `json:"manifest"`
}

func decodeHelmRelease(raw []byte) (*helmRelease, error) {
	// helm v3 stores releases as base64(gzip(json(release))). The
	// outer Secret base64 is already decoded by client-go, so we only
	// peel base64 once + gzip once.
	dec, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	plain, err := gunzip(dec)
	if err != nil {
		return nil, err
	}
	rel := &helmRelease{}
	if err := json.Unmarshal(plain, rel); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return rel, nil
}

func gunzip(in []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(in))
	if err != nil {
		return nil, fmt.Errorf("gzip header: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("gzip body: %w", err)
	}
	return out, nil
}
