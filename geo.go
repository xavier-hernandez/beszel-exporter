package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const geoAPI = "https://xapi.xavier.cc/Ip/"

type geoResult struct {
	Country *string `json:"country"`
	State   *string `json:"state"`
	City    *string `json:"city"`
}

// label joins the non-empty parts as "city, state, country".
func (g geoResult) label() string {
	var parts []string
	for _, p := range []*string{g.City, g.State, g.Country} {
		if p != nil && strings.TrimSpace(*p) != "" {
			parts = append(parts, strings.TrimSpace(*p))
		}
	}
	return strings.Join(parts, ", ")
}

// fillLocations looks up each system's location from its host address.
// Hostnames are resolved to an IP first; each IP is queried only once.
func fillLocations(systems []*system) {
	client := &http.Client{Timeout: 10 * time.Second}

	ipFor := map[*system]string{}
	unique := map[string]string{}
	for _, s := range systems {
		if ip := resolveIP(s.Host); ip != "" {
			ipFor[s] = ip
			unique[ip] = ""
		} else {
			log.Printf("location: cannot resolve %q for %s", s.Host, s.Name)
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // limit concurrent API calls
	for ip := range unique {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			loc, err := lookupLocation(client, ip)
			if err != nil {
				log.Printf("location: %s: %v", ip, err)
				return
			}
			mu.Lock()
			unique[ip] = loc
			mu.Unlock()
		}(ip)
	}
	wg.Wait()

	for s, ip := range ipFor {
		s.Location = unique[ip]
	}
}

func resolveIP(host string) string {
	host = strings.TrimSpace(host)
	if net.ParseIP(host) != nil {
		return host
	}
	if host == "" || strings.HasPrefix(host, "/") { // empty or unix socket path
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	return addrs[0].IP.String()
}

func lookupLocation(client *http.Client, ip string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, geoAPI+url.PathEscape(ip), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "*/*")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %s", resp.Status)
	}
	var g geoResult
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return g.label(), nil
}
