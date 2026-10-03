// Central labels: the dashboard API and its peer endpoint.
//
//	GET  /api/services/{svc}/labels[?host=]  central map, effective keys,
//	                                         every host's container labels,
//	                                         drift, import preview +
//	                                         conflicts, applied versions
//	POST /api/services/{svc}/labels[?host=]  {if_version, request_id, set,
//	                                         unset, allow_loosen,
//	                                         resolve_conflicts}
//	GET  /peer/labels/{svc}                  this host's view only
//	POST /peer/labels/{svc}                  a peer's forwarded POST
//
// Labels live in the shared Redis, so a write lands for every host at once;
// ?host= only picks whose Docker state answers (self-guard, canary check,
// import). A write never recreates anything — the proxies pick the new
// version up within ~5s. Responses and audit entries carry key names and
// versions; label values are not secrets but are kept out of the audit log.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/httpx"
	"github.com/PolarBaeJr/proxy-manager/internal/labels"
)

// labelsPeerTimeout bounds one peer view fetch or forwarded write.
var labelsPeerTimeout = 10 * time.Second

// labelsProxyTimeout bounds one GET of a proxy's /labels.
var labelsProxyTimeout = 3 * time.Second

var (
	errLabelsDisabled   = errors.New("central labels are not enabled on this host (LABELS_CENTRAL)")
	errLabelsWritesOff  = errors.New("central label writes are disabled on this host (LABELS_WRITES) — no change made")
	errLabelsNotAdopted = errors.New("labels are not centrally managed")
	errLabelsNotFound   = errors.New("service not found on any reachable host")
	errLabelsSelf       = errors.New("refusing to manage the dashboard's own service from within itself")
)

// errLabelsSelfUnknown: the self-guard could not ask Docker. Unlike the
// generic service guard, a label write refuses rather than assuming "not
// self".
type errLabelsSelfUnknown struct{ err error }

func (e errLabelsSelfUnknown) Error() string {
	return "cannot verify this isn't the dashboard's own service (docker: " + e.err.Error() + ") — no change made"
}

// errLabelsSeedUnverifiable: Redis lost the global version, the local proxy
// can't be asked what it was overlaying, and this dashboard never saw any
// adopted service either — a write could revert every other service.
var errLabelsSeedUnverifiable = errors.New("labels store has no " + labels.RedisVersion + " (Redis wiped?) and the local proxy's /labels is unreachable — can't tell which services were adopted, no change made")

type errLabelsBad struct{ err error }

func (e errLabelsBad) Error() string { return e.err.Error() }

type errLabelsImportConflict struct {
	Conflicts []labelConflict
	Preview   map[string]string
}

func (e errLabelsImportConflict) Error() string {
	keys := make([]string, 0, len(e.Conflicts))
	for _, c := range e.Conflicts {
		keys = append(keys, c.Key)
	}
	return "hosts/replicas disagree on " + strings.Join(keys, ", ") + " — resolve each via resolve_conflicts (value, or \"\" for unset), set or unset"
}

type labelsSetRequest struct {
	IfVersion        uint64            `json:"if_version"`
	RequestID        string            `json:"request_id,omitempty"`
	Set              map[string]string `json:"set,omitempty"`
	Unset            []string          `json:"unset,omitempty"`
	AllowLoosen      bool              `json:"allow_loosen,omitempty"`
	ResolveConflicts map[string]string `json:"resolve_conflicts,omitempty"`
}

type labelsSetResponse struct {
	Version     uint64   `json:"version"`
	ChangedKeys []string `json:"changed_keys"`
	Imported    bool     `json:"imported,omitempty"`
	NoOp        bool     `json:"no_op,omitempty"`
	Replayed    bool     `json:"replayed,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

type labelConflict struct {
	Key string `json:"key"`
	// Values: "host/member" → value ("" = unset there).
	Values map[string]string `json:"values"`
	Reason string            `json:"reason,omitempty"`
}

type labelsMemberView struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

// labelsApplied is what one proxy's GET /labels reports.
type labelsApplied struct {
	Version    uint64 `json:"version"`
	Source     string `json:"source"`
	RedisOK    bool   `json:"redis_ok"`
	HasService bool   `json:"has_service"`
	Error      string `json:"error,omitempty"`
}

// labelsHostView is one host's own facts: its members' RAW managed labels,
// non-settable proxy.* keys, staged canaries, and its proxy's applied state.
type labelsHostView struct {
	Host         string             `json:"host"`
	Members      []labelsMemberView `json:"members"`
	Canaries     int                `json:"canaries,omitempty"`
	ReadonlyKeys []string           `json:"readonly_keys,omitempty"`
	Applied      *labelsApplied     `json:"applied,omitempty"`
	Error        string             `json:"error,omitempty"`
}

type labelDrift struct {
	Member    string `json:"member"`
	Key       string `json:"key"`
	Container string `json:"container"`
	Central   string `json:"central"`
}

type labelsStoreStatus struct {
	Enabled       bool   `json:"enabled"`
	WritesEnabled bool   `json:"writes_enabled"`
	RedisOK       bool   `json:"redis_ok"`
	EverLoaded    bool   `json:"ever_loaded"`
	GlobalVersion uint64 `json:"global_version"`
	Wiped         bool   `json:"wiped,omitempty"`
}

type labelsView struct {
	Service         string                                  `json:"service"`
	Managed         bool                                    `json:"managed"`
	Version         uint64                                  `json:"version"`
	Labels          map[string]string                       `json:"labels"`
	Effective       map[string]string                       `json:"effective"`
	ContainerLabels map[string]map[string]map[string]string `json:"container_labels"`
	Drift           map[string][]labelDrift                 `json:"drift,omitempty"`
	ImportPreview   map[string]string                       `json:"import_preview,omitempty"`
	Conflicts       []labelConflict                         `json:"conflicts,omitempty"`
	ReadonlyKeys    []string                                `json:"readonly_keys"`
	LiveKeys        []string                                `json:"live_keys"`
	Applied         map[string]*labelsApplied               `json:"applied"`
	AdoptedAt       string                                  `json:"adopted_at,omitempty"`
	AdoptedBy       string                                  `json:"adopted_by,omitempty"`
	UpdatedAt       string                                  `json:"updated_at,omitempty"`
	UpdatedBy       string                                  `json:"updated_by,omitempty"`
	Store           labelsStoreStatus                       `json:"store"`
	Warnings        []string                                `json:"warnings,omitempty"`
}

func labelsIdentity(registry *PeerRegistry) string {
	if registry != nil && registry.Identity() != "" {
		return registry.Identity()
	}
	return "local"
}

func labelsProxyURL() string {
	if u := proxyURLFromEnv(); u != "" {
		return u
	}
	return defaultProxyURL
}

// proxyLabelsStatus is the proxy's GET /labels body (cmd/proxy labelsStatus).
type proxyLabelsStatus struct {
	Version  uint64                       `json:"version"`
	Source   string                       `json:"source"`
	RedisOK  bool                         `json:"redis_ok"`
	Services map[string]map[string]string `json:"services"`
}

func fetchProxyLabels(ctx context.Context, proxyURL string) (proxyLabelsStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, labelsProxyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(proxyURL, "/")+"/labels", nil)
	if err != nil {
		return proxyLabelsStatus{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return proxyLabelsStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return proxyLabelsStatus{}, fmt.Errorf("proxy /labels: %s", resp.Status)
	}
	var st proxyLabelsStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return proxyLabelsStatus{}, err
	}
	return st, nil
}

func managedOnly(raw map[string]string) map[string]string {
	out := map[string]string{}
	for _, k := range managedLabelKeys {
		if v, ok := raw[k]; ok {
			out[k] = v
		}
	}
	return out
}

// buildLabelsHostView is this host's own facts about svc, from RAW labels.
func buildLabelsHostView(ctx context.Context, dc *dockerClient, identity, svc string) labelsHostView {
	v := labelsHostView{Host: identity, Members: []labelsMemberView{}}
	all, err := dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, svc))
	if err != nil {
		v.Error = "docker: " + err.Error()
		return v
	}
	v.Canaries = len(canaryOnly(all))
	readonly := map[string]bool{}
	for _, ct := range preferRunning(liveOnly(all)) {
		v.Members = append(v.Members, labelsMemberView{Name: ct.name(), Labels: managedOnly(ct.Labels)})
		for k := range ct.Labels {
			if (strings.HasPrefix(k, "proxy.") || strings.HasPrefix(k, "pmgr.")) && !labels.Managed(k) {
				readonly[k] = true
			}
		}
	}
	sort.Slice(v.Members, func(i, j int) bool { return v.Members[i].Name < v.Members[j].Name })
	for k := range readonly {
		v.ReadonlyKeys = append(v.ReadonlyKeys, k)
	}
	sort.Strings(v.ReadonlyKeys)
	ap := &labelsApplied{}
	if st, err := fetchProxyLabels(ctx, labelsProxyURL()); err != nil {
		ap.Error = "local proxy /labels unreachable"
	} else {
		_, has := st.Services[svc]
		ap = &labelsApplied{Version: st.Version, Source: st.Source, RedisOK: st.RedisOK, HasService: has}
	}
	v.Applied = ap
	return v
}

// gatherLabelsHostViews is this host's view plus every reachable peer's.
// Unreachable peers become warnings — their containers were not compared.
func gatherLabelsHostViews(ctx context.Context, dc *dockerClient, registry *PeerRegistry, svc string) ([]labelsHostView, []string) {
	views := []labelsHostView{buildLabelsHostView(ctx, dc, labelsIdentity(registry), svc)}
	secret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
	if registry == nil || secret == "" {
		return views, nil
	}
	status := registry.Status()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var warnings []string
	client := &http.Client{Timeout: labelsPeerTimeout}
	for peerURL, st := range status {
		if st.Identity == "" {
			continue
		}
		wg.Add(1)
		go func(peerURL, identity string) {
			defer wg.Done()
			var pv labelsHostView
			err := peerGETTimeout(ctx, client, peerURL, secret, "/peer/labels/"+url.PathEscape(svc), labelsPeerTimeout, &pv)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("host %s did not answer — its containers were not compared", identity))
				return
			}
			pv.Host = identity
			views = append(views, pv)
		}(peerURL, st.Identity)
	}
	wg.Wait()
	sort.Slice(views[1:], func(i, j int) bool { return views[1+i].Host < views[1+j].Host })
	sort.Strings(warnings)
	return views, warnings
}

type labelsImport struct {
	Preview   map[string]string
	Conflicts []labelConflict
	PerHost   map[string]map[string]string
	Members   int
}

// computeLabelsImport merges every member's RAW managed labels across hosts.
// A key every member agrees on (set to one valid value, or unset everywhere)
// imports as-is; disagreement or an invalid value is a conflict.
func computeLabelsImport(views []labelsHostView) labelsImport {
	imp := labelsImport{Preview: map[string]string{}, PerHost: map[string]map[string]string{}}
	for _, hv := range views {
		if len(hv.Members) > 0 {
			imp.PerHost[hv.Host] = hv.Members[0].Labels
		}
		imp.Members += len(hv.Members)
	}
	for _, k := range managedLabelKeys {
		values := map[string]string{}
		distinct := map[string]bool{}
		for _, hv := range views {
			for _, m := range hv.Members {
				values[hv.Host+"/"+m.Name] = m.Labels[k]
				distinct[m.Labels[k]] = true
			}
		}
		if len(distinct) > 1 {
			imp.Conflicts = append(imp.Conflicts, labelConflict{Key: k, Values: values})
			continue
		}
		for v := range distinct {
			if v == "" {
				continue
			}
			if err := labels.ValidateValue(k, v); err != nil {
				imp.Conflicts = append(imp.Conflicts, labelConflict{Key: k, Values: values, Reason: err.Error()})
				continue
			}
			imp.Preview[k] = v
		}
	}
	return imp
}

// resolveLabelsImport is the base map a first adopt writes before set/unset:
// the preview plus every conflict resolved by resolve_conflicts, set or unset.
func resolveLabelsImport(imp labelsImport, req labelsSetRequest) (map[string]string, error) {
	base := map[string]string{}
	for k, v := range imp.Preview {
		base[k] = v
	}
	conflicting := map[string]bool{}
	var unresolved []labelConflict
	for _, c := range imp.Conflicts {
		conflicting[c.Key] = true
		if v, ok := req.ResolveConflicts[c.Key]; ok {
			if v == "" {
				delete(base, c.Key)
				continue
			}
			if err := labels.ValidateValue(c.Key, v); err != nil {
				return nil, errLabelsBad{err}
			}
			base[c.Key] = v
			continue
		}
		if _, ok := req.Set[c.Key]; ok || containsString(req.Unset, c.Key) {
			continue
		}
		unresolved = append(unresolved, c)
	}
	for k := range req.ResolveConflicts {
		if !conflicting[k] {
			return nil, errLabelsBad{fmt.Errorf("resolve_conflicts: %s is not in conflict — use set/unset", k)}
		}
	}
	if len(unresolved) > 0 {
		return nil, errLabelsImportConflict{Conflicts: unresolved, Preview: imp.Preview}
	}
	return base, nil
}

// ensureLabelsSeeded is the wipe guard every write runs first. With
// pmgr:labels:version missing, a single-service write would recreate the
// index holding only that service and revert every other adopted service in
// every proxy. So: reseed from the local proxy's overlay (what it is still
// routing on), else from this dashboard's own snapshot, else — if both are
// known to be empty — this is a genuine first write and may create the
// counter. allowInit reports that last case.
func ensureLabelsSeeded(ctx context.Context, dc *dockerClient, r *http.Request, actor string) (allowInit bool, warnings []string, err error) {
	_, exists, err := dc.labels.GlobalVersion(ctx)
	if err != nil {
		log.Printf("central labels: global version: %v", err)
		return false, nil, errLabelsUnavailable
	}
	if exists {
		return false, nil, nil
	}
	var services map[string]map[string]string
	from := ""
	st, perr := fetchProxyLabels(ctx, labelsProxyURL())
	switch {
	case perr == nil && len(st.Services) > 0:
		services, from = st.Services, "proxy"
	case dc.labels.Snapshot() != nil && len(dc.labels.Snapshot().Services) > 0:
		services, from = dc.labels.Snapshot().Services, "snapshot"
	case perr != nil:
		return false, nil, errLabelsSeedUnverifiable
	default:
		return true, nil, nil
	}
	names := make([]string, 0, len(services))
	for svc := range services {
		if labels.ValidService(svc) {
			names = append(names, svc)
		}
	}
	sort.Strings(names)
	var seeded []string
	for _, svc := range names {
		_, werr := dc.labels.Write(ctx, labelWrite{Service: svc, IfVersion: 0, Labels: sanitizeLabelMap(svc, services[svc]), Actor: "reseed-from-" + from, AllowInit: true})
		var conflict errLabelsVersionConflict
		switch {
		case werr == nil:
			seeded = append(seeded, svc)
		case errors.As(werr, &conflict):
			// Already back (another dashboard reseeded it first).
		default:
			log.Printf("central labels: reseed %s: %v", svc, werr)
			return false, nil, errLabelsUnavailable
		}
	}
	audit(r, actor, "labels_reseed_from_"+from, fmt.Sprintf("%d service(s): %s", len(seeded), strings.Join(seeded, ",")))
	log.Printf("central labels: Redis had no %s — reseeded %d service(s) from the %s", labels.RedisVersion, len(seeded), from)
	dc.labels.Refresh(ctx)
	// Every known service is now restored or found intact (a conflict means
	// its meta survived), so this write may (re)create the global counter
	// itself — otherwise a wipe of only pmgr:labels:version, with every meta
	// intact, would 503 forever. The target's own CAS still guards it.
	return true, []string{fmt.Sprintf("Redis had lost the central labels; reseeded %d service(s) from the %s before this write", len(seeded), from)}, nil
}

// commitLabelsChange is the one write path — the API, the peer endpoint and
// redirectLabelSetter all land here. importFn (nil from the setter
// redirect) builds the first-adopt import; without it an unadopted service
// is errLabelsNotAdopted.
func commitLabelsChange(ctx context.Context, dc *dockerClient, r *http.Request, svc string, req labelsSetRequest, importFn func(context.Context) (labelsImport, []string, error), actor, via string) (labelsSetResponse, error) {
	if dc.labels == nil {
		return labelsSetResponse{}, errLabelsDisabled
	}
	if !dc.labelsWrites {
		return labelsSetResponse{}, errLabelsWritesOff
	}
	if !labels.ValidService(svc) {
		return labelsSetResponse{}, errLabelsBad{fmt.Errorf("invalid service name %q (index and version are reserved)", svc)}
	}
	allowInit, warnings, err := ensureLabelsSeeded(ctx, dc, r, actor)
	if err != nil {
		return labelsSetResponse{}, err
	}
	rec, exists, err := dc.labels.Get(ctx, svc)
	if err != nil {
		log.Printf("central labels: get %s: %v", svc, err)
		return labelsSetResponse{}, errLabelsUnavailable
	}
	if exists && req.IfVersion < rec.Version && req.RequestID != "" {
		// Maybe a retry of a write that already landed: the script replays a
		// known request_id before its CAS, and with a stale if_version it can
		// never write.
		res, err := dc.labels.Write(ctx, labelWrite{Service: svc, IfVersion: req.IfVersion, RequestID: req.RequestID, Labels: rec.Labels, Actor: actor})
		if err != nil {
			return labelsSetResponse{}, err
		}
		return labelsSetResponse{Version: res.Version, ChangedKeys: []string{}, Replayed: true}, nil
	}
	var base map[string]string
	var imported map[string]map[string]string
	if exists {
		if req.IfVersion != rec.Version {
			return labelsSetResponse{}, errLabelsVersionConflict{Current: rec.Version}
		}
		if len(req.ResolveConflicts) > 0 {
			return labelsSetResponse{}, errLabelsBad{errors.New("resolve_conflicts only applies to the first adopt")}
		}
		base = rec.Labels
	} else {
		if importFn == nil {
			return labelsSetResponse{}, errLabelsNotAdopted
		}
		if req.IfVersion != 0 {
			return labelsSetResponse{}, errLabelsVersionConflict{Current: 0}
		}
		imp, iw, err := importFn(ctx)
		if err != nil {
			return labelsSetResponse{}, err
		}
		warnings = append(warnings, iw...)
		if base, err = resolveLabelsImport(imp, req); err != nil {
			return labelsSetResponse{}, err
		}
		imported = imp.PerHost
		warnings = append(warnings, "adopted: compose/container edits to "+svc+"'s managed keys are now IGNORED — change them here; GET .../labels shows drift")
	}
	if err := labels.ValidateChange(base, req.Set, req.Unset, req.AllowLoosen); err != nil {
		return labelsSetResponse{}, errLabelsBad{err}
	}
	next := make(map[string]string, len(base)+len(req.Set))
	for k, v := range base {
		next[k] = v
	}
	for _, k := range req.Unset {
		delete(next, k)
	}
	for k, v := range req.Set {
		next[k] = v
	}
	var compare map[string]string
	if exists {
		compare = base
	} else {
		compare = managedUnion(imported)
	}
	changed := changedLabelKeys(compare, next)
	if exists && len(changed) == 0 {
		return labelsSetResponse{Version: rec.Version, ChangedKeys: []string{}, NoOp: true, Warnings: warnings}, nil
	}
	res, err := dc.labels.Write(ctx, labelWrite{Service: svc, IfVersion: req.IfVersion, RequestID: req.RequestID, Labels: next, Actor: actor, Imported: imported, AllowInit: allowInit})
	if err != nil {
		if errors.Is(err, errLabelsUnavailable) {
			log.Printf("central labels: write %s: %v", svc, err)
			return labelsSetResponse{}, errLabelsUnavailable
		}
		return labelsSetResponse{}, err
	}
	if containsString(changed, labelDrain) {
		warnings = append(warnings, "proxy.drain applies to the dashboard's stops immediately; the containers' own Docker StopTimeout (plain docker stop, daemon shutdown) keeps the value from their container label")
	}
	target := fmt.Sprintf("%s v%d: set %s unset %s", svc, res.Version, strings.Join(sortedKeys(req.Set), ","), strings.Join(req.Unset, ","))
	if !exists {
		target += " (import)"
	}
	if via != "" {
		target += " (via " + via + ")"
	}
	audit(r, actor, "service.labels_set", target)
	dc.labels.Refresh(ctx)
	return labelsSetResponse{Version: res.Version, ChangedKeys: changed, Imported: !exists, Replayed: res.Replayed, Warnings: warnings}, nil
}

// managedUnion: for the import's changed_keys, the first host's template
// stands in for "what was in effect".
func managedUnion(perHost map[string]map[string]string) map[string]string {
	hosts := make([]string, 0, len(perHost))
	for h := range perHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	if len(hosts) == 0 {
		return map[string]string{}
	}
	return perHost[hosts[0]]
}

func changedLabelKeys(a, b map[string]string) []string {
	out := []string{}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func writeLabelsErr(w http.ResponseWriter, err error) {
	var conflict errLabelsVersionConflict
	var imp errLabelsImportConflict
	var bad errLabelsBad
	var selfUnknown errLabelsSelfUnknown
	switch {
	case errors.Is(err, errLabelsSelf):
		http.Error(w, err.Error()+" — use docker compose on the host", http.StatusForbidden)
	case errors.As(err, &selfUnknown):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, errLabelsUnavailable):
		http.Error(w, errLabelsUnavailable.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, errLabelsWiped), errors.Is(err, errLabelsSeedUnverifiable), errors.Is(err, errLabelsDisabled):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, errLabelsWritesOff):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.As(err, &conflict):
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "current_version": conflict.Current})
	case errors.As(err, &imp):
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "conflicts": imp.Conflicts, "import_preview": imp.Preview})
	case errors.Is(err, errLabelsNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.As(err, &bad):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

// labelsImportFn builds the first-adopt import from every reachable host.
// Adoption needs at least one member somewhere; members are found by
// proxy.service, so every one carries it (the proxy overlay's join key).
func labelsImportFn(dc *dockerClient, registry *PeerRegistry, svc string) func(context.Context) (labelsImport, []string, error) {
	return func(ctx context.Context) (labelsImport, []string, error) {
		views, warnings := gatherLabelsHostViews(ctx, dc, registry, svc)
		for _, v := range views {
			if v.Error != "" {
				return labelsImport{}, nil, fmt.Errorf("host %s: %s", v.Host, v.Error)
			}
		}
		imp := computeLabelsImport(views)
		if imp.Members == 0 {
			return labelsImport{}, nil, errLabelsNotFound
		}
		return imp, warnings, nil
	}
}

// serveLabelsSetLocal runs a POST against this host: self-guard, canary
// weight check, then the shared write path. Shared by the API and the peer
// endpoint so a forwarded write answers exactly like a local one.
//
// No rollout guard on purpose: a label write recreates nothing, so it can't
// race a rollout's or rolling replace's container manipulation — except
// proxy.weight while a canary is staged, which would split the group's
// advertised weight (see setWeightLabel).
func serveLabelsSetLocal(w http.ResponseWriter, r *http.Request, dc *dockerClient, registry *PeerRegistry, svc string, req labelsSetRequest, actor, via string) {
	if err := labelsGuards(r.Context(), dc, svc, req); err != nil {
		writeLabelsErr(w, err)
		return
	}
	res, err := commitLabelsChange(r.Context(), dc, r, svc, req, labelsImportFn(dc, registry, svc), actor, via)
	if err != nil {
		writeLabelsErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// labelsGuards: self-guard first (403 on self AND on a Docker error), then
// the staged-canary weight refusal.
func labelsGuards(ctx context.Context, dc *dockerClient, svc string, req labelsSetRequest) error {
	self, err := dc.serviceContainsSelfByName(ctx, svc)
	if err != nil {
		return errLabelsSelfUnknown{err}
	}
	if self {
		return errLabelsSelf
	}
	if _, ok := req.Set[labelWeight]; ok || containsString(req.Unset, labelWeight) || req.ResolveConflicts[labelWeight] != "" {
		all, err := dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, svc))
		if err != nil {
			return err
		}
		if len(canaryOnly(all)) > 0 {
			return errLabelsBad{fmt.Errorf("service %q has a staged canary — promote or discard it before retuning the weight", svc)}
		}
	}
	return nil
}

// buildLabelsView is the full GET answer.
func buildLabelsView(ctx context.Context, dc *dockerClient, registry *PeerRegistry, svc string) (labelsView, error) {
	v := labelsView{Service: svc, LiveKeys: managedLabelKeys, ContainerLabels: map[string]map[string]map[string]string{}, Applied: map[string]*labelsApplied{}}
	v.Store = labelsStoreStatus{Enabled: dc.labels != nil, WritesEnabled: dc.labels != nil && dc.labelsWrites}
	var rec labelRecord
	if dc.labels != nil {
		v.Store.RedisOK, v.Store.EverLoaded = dc.labels.RedisOK(), dc.labels.EverLoaded()
		gv, exists, err := dc.labels.GlobalVersion(ctx)
		switch {
		case err != nil:
			v.Store.RedisOK = false
			v.Warnings = append(v.Warnings, errLabelsUnavailable.Error()+" — showing the last snapshot")
			if m, ok := dc.adoptedLabels(svc); ok {
				v.Managed, rec.Labels = true, m
			}
		default:
			v.Store.GlobalVersion, v.Store.Wiped = gv, !exists
			r, ok, err := dc.labels.Get(ctx, svc)
			if err != nil {
				v.Warnings = append(v.Warnings, errLabelsUnavailable.Error())
			} else if ok {
				v.Managed, rec = true, r
			} else if m, ok := dc.adoptedLabels(svc); ok && !exists {
				v.Managed, rec.Labels = true, m
				v.Warnings = append(v.Warnings, "Redis lost the central labels; showing the last snapshot — the next write reseeds it")
			}
		}
	}
	views, warnings := gatherLabelsHostViews(ctx, dc, registry, svc)
	v.Warnings = append(v.Warnings, warnings...)
	readonly := map[string]bool{}
	members := 0
	for _, hv := range views {
		if hv.Error != "" {
			v.Warnings = append(v.Warnings, fmt.Sprintf("host %s: %s", hv.Host, hv.Error))
		}
		byMember := map[string]map[string]string{}
		for _, m := range hv.Members {
			byMember[m.Name] = m.Labels
		}
		v.ContainerLabels[hv.Host] = byMember
		members += len(hv.Members)
		for _, k := range hv.ReadonlyKeys {
			readonly[k] = true
		}
		if hv.Applied != nil {
			v.Applied[hv.Host] = hv.Applied
		}
	}
	if members == 0 && !v.Managed {
		return labelsView{}, errLabelsNotFound
	}
	v.ReadonlyKeys = []string{}
	for k := range readonly {
		v.ReadonlyKeys = append(v.ReadonlyKeys, k)
	}
	sort.Strings(v.ReadonlyKeys)
	if v.Managed {
		v.Version, v.Labels = rec.Version, rec.Labels
		if v.Labels == nil {
			v.Labels = map[string]string{}
		}
		v.AdoptedAt, v.AdoptedBy, v.UpdatedAt, v.UpdatedBy = rec.AdoptedAt, rec.AdoptedBy, rec.UpdatedAt, rec.UpdatedBy
		v.Effective = v.Labels
		v.Drift = map[string][]labelDrift{}
		for _, hv := range views {
			for _, m := range hv.Members {
				for _, k := range managedLabelKeys {
					if m.Labels[k] != v.Labels[k] {
						v.Drift[hv.Host] = append(v.Drift[hv.Host], labelDrift{Member: m.Name, Key: k, Container: m.Labels[k], Central: v.Labels[k]})
					}
				}
			}
		}
		if len(v.Drift) == 0 {
			v.Drift = nil
		}
	} else {
		imp := computeLabelsImport(views)
		v.ImportPreview, v.Conflicts = imp.Preview, imp.Conflicts
		if len(views) > 0 && len(views[0].Members) > 0 {
			v.Effective = views[0].Members[0].Labels
		}
	}
	if v.Effective == nil {
		v.Effective = map[string]string{}
	}
	return v, nil
}

// forwardLabels relays a GET (the peer's own host view) or POST to the
// peer named by host. The peer's 403 (self-guard) and 404 (unsupported) are
// re-mapped: to the UI a 401/403 means "session expired" / "2FA required".
func forwardLabels(w http.ResponseWriter, r *http.Request, registry *PeerRegistry, host, svc string, body []byte, actor, requestID string) {
	secret := strings.TrimSpace(os.Getenv("DASHBOARD_PEER_SECRET"))
	if registry == nil || secret == "" {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	peerURL, ok := registry.URLForIdentity(host)
	if !ok {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	client := &http.Client{Timeout: labelsPeerTimeout}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	code, respBody, err := peerMutate(r.Context(), client, peerURL, secret, r.Method, "/peer/labels/"+url.PathEscape(svc), labelsPeerTimeout, rdr, nil, mintForwardedActor(r, actor))
	if err != nil {
		switch {
		case code == 0 && isDialError(err):
			http.Error(w, host+" is unreachable — nothing was sent", http.StatusServiceUnavailable)
		case r.Method == http.MethodGet:
			http.Error(w, host+" did not answer in time", http.StatusGatewayTimeout)
		default:
			httpx.WriteJSON(w, http.StatusGatewayTimeout, map[string]any{"error": host + " did not answer in time — outcome unknown; retry with the same request_id", "request_id": requestID})
		}
		return
	}
	peerText := strings.TrimSpace(string(respBody))
	switch code {
	case http.StatusOK, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable:
		writePeerRelay(w, code, respBody)
	case http.StatusNotFound:
		if strings.Contains(peerText, errLabelsNotFound.Error()) {
			http.Error(w, peerText, http.StatusNotFound)
			return
		}
		http.Error(w, "peer "+host+" does not support central labels — deploy the new dashboard there (and set DASHBOARD_PEER_SECRET / -peer-writes)", http.StatusConflict)
	case http.StatusForbidden:
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{"error": peerText})
	case http.StatusUnauthorized:
		http.Error(w, "peer "+host+" rejected mesh credentials", http.StatusBadGateway)
	default:
		http.Error(w, fmt.Sprintf("labels on %s failed (status %d): %s", host, code, peerText), http.StatusBadGateway)
	}
}

// serveLabelsAPI handles /api/services/{svc}/labels. Dispatched before the
// generic ?host= forwarding, self-guard and rollout guard in api.go.
func serveLabelsAPI(w http.ResponseWriter, r *http.Request, dc *dockerClient, auth *AuthStore, registry *PeerRegistry, svc string) {
	if !labels.ValidService(svc) {
		http.Error(w, fmt.Sprintf("invalid service name %q (index and version are reserved)", svc), http.StatusBadRequest)
		return
	}
	host, isPeer := hostForReq(r, registry)
	switch r.Method {
	case http.MethodGet:
		if isPeer {
			forwardLabels(w, r, registry, host, svc, nil, "", "")
			return
		}
		v, err := buildLabelsView(r.Context(), dc, registry, svc)
		if err != nil {
			writeLabelsErr(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, v)
	case http.MethodPost:
		var req labelsSetRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Labels are shared by every host, so the guards run here even when
		// the write is forwarded (the peer runs its own too).
		if err := labelsGuards(r.Context(), dc, svc, req); err != nil {
			writeLabelsErr(w, err)
			return
		}
		actor := auditActor(auth, r)
		if isPeer {
			body, _ := json.Marshal(req)
			forwardLabels(w, r, registry, host, svc, body, actor, req.RequestID)
			return
		}
		res, err := commitLabelsChange(r.Context(), dc, r, svc, req, labelsImportFn(dc, registry, svc), actor, "")
		if err != nil {
			writeLabelsErr(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, res)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// peerLabelsHandler serves /peer/labels/{svc}: GET is this host's own view
// (never asks further peers), POST a peer's forwarded write — needs
// -peer-writes, runs this host's own guards, audited as peer-mesh. 404 unless
// the mesh secret is set, like every /peer/ endpoint.
func peerLabelsHandler(secret string, dc *dockerClient, registry *PeerRegistry, writesEnabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc := strings.TrimPrefix(r.URL.Path, "/peer/labels/")
		if secret == "" || (r.Method == http.MethodPost && !writesEnabled) || !labels.ValidService(svc) {
			http.NotFound(w, r)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			httpx.WriteJSON(w, http.StatusOK, buildLabelsHostView(r.Context(), dc, labelsIdentity(registry), svc))
		case http.MethodPost:
			var req labelsSetRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
				return
			}
			serveLabelsSetLocal(w, r, dc, registry, svc, req, "peer-mesh", "forwarded")
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
