package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultRequestTimeout bounds every GATE request (read or write).
const defaultRequestTimeout = 10 * time.Second

// gateClient posts JSON bodies to one GATE endpoint.
type gateClient struct {
	url    string
	client *http.Client
}

func newGateClient(url string, timeout time.Duration) *gateClient {
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	return &gateClient{url: url, client: &http.Client{Timeout: timeout}}
}

// post sends body and returns the response body. Transport errors, timeouts, non-2xx
// statuses and failures to read the body are all returned as an error: the caller cannot
// tell whether the GATE acted on the request.
func (c *gateClient) post(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("non-success response from GATE: %s", resp.Status)
	}
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	return data, nil
}

// entryKey is the mailbox and registry key of a device: Clu + Id, case-insensitive.
func entryKey(clu, id string) string {
	return strings.ToLower(clu) + "|" + strings.ToLower(id)
}

// decodeReadResponse matches the items of a read response (a JSON array, as read-script.lua
// answers) to the entries of the request by Clu + Id. An entry without an item is missing.
// A body that is not an array of objects fails the whole request.
func decodeReadResponse(entries []*laneEntry, body []byte) ([]laneResult, error) {
	var items []ReqObject
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("unreadable read response: %w", err)
	}
	byKey := make(map[string]int, len(items))
	for i, it := range items {
		k := entryKey(it.Clu, it.Id)
		if _, dup := byKey[k]; !dup {
			byKey[k] = i
		}
	}
	out := make([]laneResult, len(entries))
	for i, e := range entries {
		idx, ok := byKey[e.key]
		if !ok {
			out[i] = laneResult{Outcome: OutcomeMissing, Err: fmt.Errorf("%s|%s not in read response", e.obj.Clu, e.obj.Id)}
			continue
		}
		item := items[idx]
		out[i] = laneResult{Outcome: OutcomeOK, Item: &item}
	}
	return out, nil
}

// writeAck is one acknowledgement item of a write response. The old update-script.lua
// answers with a read-back payload instead, which is ignored: a write response is never
// loaded as device state.
type writeAck struct {
	Clu   string
	Id    string
	Kind  string
	Error *string
}

// decodeWriteResponse turns a write response into one result per entry. It accepts both
// shapes of concept 3.7: a single object (old script, or new script answering an object
// request) and an array of acknowledgements. Items are matched by Clu + Id.
//
//   - item without Error: acknowledged;
//   - item with Error: per-item error (the command did not run);
//   - object with Error and no identity: the whole request failed, every entry gets that error;
//   - entry without an item (including the old script's "{}" for an unknown CLU): uncertain;
//   - a body that is neither an object nor an array of objects: error returned, the caller
//     reports every entry as uncertain.
func decodeWriteResponse(entries []*laneEntry, body []byte) ([]laneResult, error) {
	trimmed := bytes.TrimSpace(body)
	var acks []writeAck
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &acks); err != nil {
			return nil, fmt.Errorf("unreadable write response: %w", err)
		}
	default:
		var one writeAck
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, fmt.Errorf("unreadable write response: %w", err)
		}
		if one.Clu == "" && one.Id == "" && one.Error != nil {
			out := make([]laneResult, len(entries))
			for i := range entries {
				out[i] = laneResult{Outcome: OutcomeError, Err: fmt.Errorf("GATE rejected the request: %s", *one.Error)}
			}
			return out, nil
		}
		acks = []writeAck{one}
	}

	byKey := make(map[string]writeAck, len(acks))
	for _, a := range acks {
		if a.Clu == "" && a.Id == "" {
			continue
		}
		k := entryKey(a.Clu, a.Id)
		if _, dup := byKey[k]; !dup {
			byKey[k] = a
		}
	}
	out := make([]laneResult, len(entries))
	for i, e := range entries {
		a, ok := byKey[e.key]
		switch {
		case !ok:
			out[i] = laneResult{Outcome: OutcomeUncertain, Err: fmt.Errorf("%s|%s not acknowledged in write response", e.obj.Clu, e.obj.Id)}
		case a.Error != nil:
			out[i] = laneResult{Outcome: OutcomeError, Err: fmt.Errorf("GATE error for %s|%s: %s", e.obj.Clu, e.obj.Id, *a.Error)}
		default:
			out[i] = laneResult{Outcome: OutcomeOK}
		}
	}
	return out, nil
}
