package summarizer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

func validateProviderBaseURL(rawURL string, allowPrivateHosts bool) (string, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid provider base URL %q", rawURL)
	}
	if !allowPrivateHosts {
		if err := rejectPrivateHost(parsed.Hostname()); err != nil {
			return "", err
		}
	}
	return baseURL, nil
}

func rejectPrivateHost(host string) error {
	host = strings.TrimSpace(strings.TrimSuffix(host, "."))
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return errors.New("provider base URL must not target localhost or private network")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateAddress(ip) {
			return errors.New("provider base URL must not target localhost or private network")
		}
		return nil
	}
	return nil
}

func isPrivateAddress(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		ip.IsMulticast() ||
		isCarrierGradeNAT(ip)
}

func secureProviderHTTPClient(client *http.Client, allowPrivateHosts bool) *http.Client {
	if allowPrivateHosts {
		return client
	}
	if client == nil {
		client = &http.Client{}
	}
	clone := *client
	transport, ok := clone.Transport.(*http.Transport)
	if !ok {
		if clone.Transport == nil {
			defaultTransport, defaultOK := http.DefaultTransport.(*http.Transport)
			if !defaultOK {
				clone.Transport = rejectingProviderTransport{}
				return &clone
			}
			transport = defaultTransport.Clone()
		} else {
			clone.Transport = rejectingProviderTransport{}
			return &clone
		}
	} else {
		transport = transport.Clone()
	}
	transport.Proxy = nil
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("split provider address: %w", err)
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve provider address: %w", err)
		}
		for _, ip := range ips {
			if isPrivateAddress(ip) {
				continue
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		return nil, errors.New("provider address resolved to a private network")
	}
	clone.Transport = transport
	previousRedirect := clone.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
			return errors.New("provider redirect to a different host is not allowed")
		}
		if err := rejectPrivateHost(req.URL.Hostname()); err != nil {
			return err
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		return nil
	}
	return &clone
}

type rejectingProviderTransport struct{}

func (rejectingProviderTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("provider HTTP client transport is not allowed")
}

func isCarrierGradeNAT(ip net.IP) bool {
	ipv4 := ip.To4()
	if ipv4 == nil {
		return false
	}
	return ipv4[0] == 100 && ipv4[1] >= 64 && ipv4[1] <= 127
}
