package main

import (
	"fmt"
	"math"
	"slices"
	"time"
)

type latencyPair struct {
	HostMS  float64 `json:"host_ms"`
	SaintMS float64 `json:"saint_ms"`
}

func elapsedMS(start time.Time) float64 {
	return float64(time.Since(start)) / float64(time.Millisecond)
}
func quantile(values []float64, q float64) (float64, error) {
	if len(values) == 0 || q <= 0 || q > 1 {
		return 0, fmt.Errorf("invalid quantile sample")
	}
	sorted := slices.Clone(values)
	for _, v := range sorted {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("invalid timing")
		}
	}
	slices.Sort(sorted)
	return sorted[int(math.Ceil(q*float64(len(sorted))))-1], nil
}
func timingResult(pairs []latencyPair, maxRatio float64) result {
	r := result{"prompt latency p50/p95", "NOT VERIFIED", "requires at least five fresh pane pairs"}
	if len(pairs) < 5 {
		return r
	}
	var host, saint []float64
	for _, p := range pairs {
		host = append(host, p.HostMS)
		saint = append(saint, p.SaintMS)
	}
	h50, e1 := quantile(host, .5)
	h95, e2 := quantile(host, .95)
	s50, e3 := quantile(saint, .5)
	s95, e4 := quantile(saint, .95)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || h50 <= 0 || h95 <= 0 || maxRatio < 1 || math.IsNaN(maxRatio) || math.IsInf(maxRatio, 0) {
		r.Status = "FAIL"
		r.Detail = "invalid startup timing samples or budget"
		return r
	}
	r.Status = "PASS"
	if s50 > h50*maxRatio || s95 > h95*maxRatio {
		r.Status = "FAIL"
	}
	r.Detail = fmt.Sprintf("CLI open-to-ready ms, n=%d: host p50=%.1f p95=%.1f; saint p50=%.1f p95=%.1f; budget %.2fx host", len(pairs), h50, h95, s50, s95, maxRatio)
	return r
}
