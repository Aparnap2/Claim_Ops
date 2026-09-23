package orchestrate

import (
	"context"
	"errors"
	"strings"
	"time"
)

// FallbackModelClient decorates primary with a secondary provider.
// It implements the bounded fallback policy for APA-13: primary is tried
// first; on retryable failure (5xx/429/transport wrapping ErrModelUpstream,
// not 4xx, not context cancellation) the secondary is tried within the
// shared MaxTotalCalls budget. Non-retryable 4xx terminal does NOT fallback.
// Cancellation propagates raw without fallback. Empty/invalid from fallback
// still flows through Loop's Decode/Validate (not accepted here).
type FallbackModelClient struct {
	Primary       ModelClient
	Secondary     ModelClient
	MaxTotalCalls int // 0 means 4 (2+2) default bound; must be >=2 if set
}

// NewFallbackModelClient builds the decorator. maxTotal 0 uses 4.
func NewFallbackModelClient(primary, secondary ModelClient, maxTotal int) *FallbackModelClient {
	if maxTotal <= 0 {
		maxTotal = 4
	}
	return &FallbackModelClient{Primary: primary, Secondary: secondary, MaxTotalCalls: maxTotal}
}

// Complete tries primary, then fallback on retryable failure within budget.
// Transport errors go directly to fallback (not primary retry) to match
// expected bounded budget for transport->fallback success (1+1=2).
// 5xx is retried once on primary (1 retry) then fallback.
func (f *FallbackModelClient) Complete(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	if ctx.Err() != nil {
		return ModelResponse{}, ctx.Err()
	}
	primaryResp, primaryErr := f.Primary.Complete(ctx, req)
	if primaryErr == nil {
		return primaryResp, nil
	}
	if ctx.Err() != nil {
		return ModelResponse{}, ctx.Err()
	}
	if errors.Is(primaryErr, context.Canceled) || errors.Is(primaryErr, context.DeadlineExceeded) {
		return ModelResponse{}, primaryErr
	}
	if !isFallbackRetryable(primaryErr) {
		return ModelResponse{}, primaryErr
	}
	msg := primaryErr.Error()
	// Transport is signaled only by the provider transport prefix
	// ("groq: do:"). Do NOT match the bare word "transport": the
	// ErrModelUpstream sentinel itself reads "... model transport
	// failure ...", so that substring matches every upstream error and
	// would suppress the 5xx primary retry.
	isTransport := strings.Contains(msg, "groq: do:")
	calls := 1
	// For 5xx, retry primary once within budget before fallback.
	if !isTransport && calls < f.MaxTotalCalls {
		select {
		case <-ctx.Done():
			return ModelResponse{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
		primaryResp2, primaryErr2 := f.Primary.Complete(ctx, req)
		calls++
		if primaryErr2 == nil {
			return primaryResp2, nil
		}
		// If second primary fails and is still retryable, fall through to fallback.
		primaryErr = primaryErr2
		primaryResp = primaryResp2
		if !isFallbackRetryable(primaryErr) {
			return ModelResponse{}, primaryErr
		}
	}
	if calls >= f.MaxTotalCalls {
		return ModelResponse{}, primaryErr
	}
	select {
	case <-ctx.Done():
		return ModelResponse{}, ctx.Err()
	default:
	}
	secondaryResp, secondaryErr := f.Secondary.Complete(ctx, req)
	if secondaryErr == nil {
		return secondaryResp, nil
	}
	if ctx.Err() != nil {
		return ModelResponse{}, ctx.Err()
	}
	// Retry secondary once if retryable and budget allows.
	if isFallbackRetryable(secondaryErr) && calls+1 < f.MaxTotalCalls {
		select {
		case <-ctx.Done():
			return ModelResponse{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
		secondaryResp2, secondaryErr2 := f.Secondary.Complete(ctx, req)
		if secondaryErr2 == nil {
			return secondaryResp2, nil
		}
		secondaryErr = secondaryErr2
	}
	if secondaryErr == nil {
		return secondaryResp, nil
	}
	return ModelResponse{}, secondaryErr
}

// isFallbackRetryable mirrors Groq's retryable classification for fallback decision.
// 5xx, 429, 408, transport (groq: do:) are retryable; 4xx (400,401,403,404) and contract/empty are not.
func isFallbackRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrModelEmpty) || errors.Is(err, ErrModelContract) {
		return false
	}
	// If it wraps Upstream, check status.
	if errors.Is(err, ErrModelUpstream) {
		msg := err.Error()
		// 4xx terminal even though Upstream — check 4xx before 5xx/429.
		if strings.Contains(msg, "status 400") || strings.Contains(msg, "status 401") || strings.Contains(msg, "status 403") || strings.Contains(msg, "status 404") {
			// But 429 is retryable even though 4xx — check 429 first.
			if strings.Contains(msg, "status 429") {
				return true
			}
			return false
		}
		if strings.Contains(msg, "status 429") || strings.Contains(msg, "status 408") || strings.Contains(msg, "status 5") {
			return true
		}
		if strings.Contains(msg, "groq: do:") {
			return true
		}
		// Plain Upstream without status (e.g., FakeModelClient) is retryable.
		return true
	}
	// Fallback string check for red helpers that wrap Upstream with status 4 but not via errors.Is
	msg := err.Error()
	if strings.Contains(msg, "status 429") {
		return true
	}
	if strings.Contains(msg, "status 400") || strings.Contains(msg, "status 401") || strings.Contains(msg, "status 403") || strings.Contains(msg, "status 404") {
		return false
	}
	if strings.Contains(msg, "status 5") || strings.Contains(msg, "groq: do:") {
		return true
	}
	return false
}
