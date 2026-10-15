package goapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"hash/crc64"
	"io"
	"math"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/goapiclient"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

// streamRangeReader opens one ranged GET per call against the peer. The hop
// count is captured when Link runs (the fs request's context) and re-applied
// to whatever context the consumer later supplies, so a cached link keeps
// counting relay transits. The apikey is attached by goapiclient
// (server-side only) and every redirect is refused.
func streamRangeReader(cli *goapiclient.Client, drive, remote string, hop int) stream.RangeReaderFunc {
	return func(ctx context.Context, hr http_range.Range) (io.ReadCloser, error) {
		if hr.Start < 0 || (hr.Length > 0 && hr.Start > math.MaxInt64-(hr.Length-1)) {
			return nil, fmt.Errorf("%w: invalid read window", goapiclient.ErrBadPeerResponse)
		}
		if hr.Length == 0 {
			return io.NopCloser(strings.NewReader("")), nil
		}
		// Cached links can be replayed under a different request: keep the
		// deeper of the link-time and the runtime chain counts so a bound is
		// never silently reset by a cache hit.
		currentHop := hop
		if runtime, ok := goapiclient.HopFromContext(ctx); ok && runtime > currentHop {
			currentHop = runtime
		}
		ctx = goapiclient.WithHop(ctx, currentHop)
		resp, err := cli.Download(ctx, drive, remote, rangeHeader(hr))
		if err != nil {
			return nil, mapErr(err)
		}
		return resp.Body, nil
	}
}

// rangeHeader translates the http_range request into a Range header. Only
// the range itself is derived — never the original request's headers, which
// a cached reader could otherwise replay for a different consumer. A full
// read sends no Range header; an open-ended read becomes "bytes=N-".
func rangeHeader(hr http_range.Range) string {
	if hr.Start == 0 && hr.Length < 0 {
		return ""
	}
	if hr.Length < 0 {
		return fmt.Sprintf("bytes=%d-", hr.Start)
	}
	return fmt.Sprintf("bytes=%d-%d", hr.Start, hr.Start+hr.Length-1)
}

// hashCounter counts and hashes the bytes actually handed to the transport,
// so the typed receipt is verified against exactly what was streamed.
type hashCounter struct {
	r   io.Reader
	n   int64
	crc hash.Hash64
	sha hash.Hash
}

func newHashCounter(r io.Reader) *hashCounter {
	return &hashCounter{
		r:   r,
		crc: crc64.New(goapiclient.CRCTable()),
		sha: sha256.New(),
	}
}

func (h *hashCounter) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.n += int64(n)
		_, _ = h.crc.Write(p[:n])
		_, _ = h.sha.Write(p[:n])
	}
	return n, err
}
