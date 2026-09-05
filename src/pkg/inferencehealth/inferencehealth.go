package inferencehealth

import (
	"errors"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ClassDNS     = "dns"
	ClassConnect = "connect"
	Class5xx     = "5xx"
	ClassAuth    = "auth"
	ClassBudget  = "budget"
	ClassOther   = "other"
)

const detailLimit = 200

type GatewayStatus struct {
	Name        string `json:"name"`
	Endpoint    string `json:"endpoint,omitempty"`
	Host        string `json:"host,omitempty"`
	ErrorClass  string `json:"error_class,omitempty"`
	HTTPStatus  int    `json:"http_status,omitempty"`
	Detail      string `json:"detail,omitempty"`
	LastErrorAt string `json:"last_error_at,omitempty"`
}

type Store struct {
	mu     sync.RWMutex
	faults map[string]GatewayStatus
}

func NewStore() *Store { return &Store{faults: map[string]GatewayStatus{}} }

func (s *Store) RecordError(name string, err error, at time.Time) {
	s.RecordEndpointError(name, "", err, at)
}

func (s *Store) RecordEndpointError(name, endpoint string, err error, at time.Time) {
	if s == nil || strings.TrimSpace(name) == "" || err == nil {
		return
	}
	class, status := ClassifyError(err)
	s.record(GatewayStatus{Name: strings.TrimSpace(name), Endpoint: safeDetail(endpoint), Host: endpointHost(endpoint, err), ErrorClass: class, HTTPStatus: status, Detail: safeDetail(err.Error()), LastErrorAt: at.UTC().Format(time.RFC3339)})
}

func (s *Store) RecordHTTPError(name string, status int, detail string, at time.Time) {
	s.RecordEndpointHTTPError(name, "", status, detail, at)
}

func (s *Store) RecordEndpointHTTPError(name, endpoint string, status int, detail string, at time.Time) {
	if s == nil || strings.TrimSpace(name) == "" {
		return
	}
	s.record(GatewayStatus{Name: strings.TrimSpace(name), Endpoint: safeDetail(endpoint), Host: endpointHost(endpoint, nil), ErrorClass: ClassifyHTTPStatus(status), HTTPStatus: status, Detail: safeDetail(detail), LastErrorAt: at.UTC().Format(time.RFC3339)})
}

func (s *Store) Clear(name string) {
	if s == nil || strings.TrimSpace(name) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.faults, strings.ToLower(strings.TrimSpace(name)))
}

func (s *Store) Snapshot() []GatewayStatus {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]GatewayStatus, 0, len(s.faults))
	for _, st := range s.faults {
		out = append(out, st)
	}
	Sort(out)
	return out
}

func (s *Store) record(st GatewayStatus) {
	if strings.TrimSpace(st.ErrorClass) == "" {
		st.ErrorClass = ClassOther
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.faults == nil {
		s.faults = map[string]GatewayStatus{}
	}
	s.faults[strings.ToLower(st.Name)] = st
}

func ClassifyHTTPStatus(status int) string {
	switch {
	case status == 401 || status == 403:
		return ClassAuth
	case status == 429:
		return ClassBudget
	case status >= 500 && status <= 599:
		return Class5xx
	default:
		return ClassOther
	}
}

func ClassifyError(err error) (string, int) {
	if err == nil {
		return "", 0
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ClassDNS, 0
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return ClassConnect, 0
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such host") || strings.Contains(msg, "server misbehaving"):
		return ClassDNS, 0
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "connect:") || strings.Contains(msg, "i/o timeout"):
		return ClassConnect, 0
	case strings.Contains(msg, "http 401"):
		return ClassAuth, 401
	case strings.Contains(msg, "http 403"):
		return ClassAuth, 403
	case strings.Contains(msg, "http 429"):
		return ClassBudget, 429
	case strings.Contains(msg, "http 5"):
		return Class5xx, 0
	default:
		return ClassOther, 0
	}
}

func Reason(st GatewayStatus) string {
	name := strings.TrimSpace(st.Name)
	if name == "" {
		name = "unknown"
	}
	switch st.ErrorClass {
	case ClassDNS:
		if host := strings.TrimSpace(st.Host); host != "" {
			return name + " endpoint " + host + " not resolvable on this cluster — set inference." + name + ".endpoint or disable"
		}
		return "inference gateway '" + name + "' unreachable (dns)"
	case ClassConnect:
		return "inference gateway '" + name + "' unreachable (connect)"
	case ClassAuth:
		if st.HTTPStatus > 0 {
			return "inference gateway '" + name + "' rejected key (" + strconv.Itoa(st.HTTPStatus) + ")"
		}
		return "inference gateway '" + name + "' rejected key"
	case ClassBudget:
		return "inference gateway '" + name + "' budget/rate limited (429)"
	case Class5xx:
		return "inference gateway '" + name + "' returned 5xx"
	default:
		return "inference gateway '" + name + "' failing"
	}
}

func Sort(st []GatewayStatus) {
	sort.Slice(st, func(i, j int) bool {
		return strings.ToLower(st[i].Name) < strings.ToLower(st[j].Name)
	})
}

func MostRecent(st []GatewayStatus) (GatewayStatus, bool) {
	var best GatewayStatus
	var bestAt time.Time
	for _, item := range st {
		if strings.TrimSpace(item.Name) == "" || strings.TrimSpace(item.ErrorClass) == "" {
			continue
		}
		at, _ := time.Parse(time.RFC3339, item.LastErrorAt)
		if best.Name == "" || at.After(bestAt) {
			best = item
			bestAt = at
		}
	}
	return best, best.Name != ""
}

func safeDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > detailLimit {
		return s[:detailLimit]
	}
	return s
}

func endpointHost(endpoint string, err error) string {
	if u, parseErr := url.Parse(strings.TrimSpace(endpoint)); parseErr == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && strings.TrimSpace(dnsErr.Name) != "" {
		return strings.TrimSpace(dnsErr.Name)
	}
	return ""
}
