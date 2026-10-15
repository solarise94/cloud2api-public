package goapi

import (
	"io"
	"math"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

func TestRangeHeaderMatchesOpenListUnlimitedLength(t *testing.T) {
	for _, tc := range []struct {
		window http_range.Range
		want   string
	}{
		{http_range.Range{Start: 0, Length: -1}, ""},
		{http_range.Range{Start: 6, Length: -1}, "bytes=6-"},
		{http_range.Range{Start: 0, Length: 11}, "bytes=0-10"},
		{http_range.Range{Start: 6, Length: 5}, "bytes=6-10"},
	} {
		if got := rangeHeader(tc.window); got != tc.want {
			t.Errorf("rangeHeader(%+v) = %q, want %q", tc.window, got, tc.want)
		}
	}
}

func TestNativeFullReadAndEmptyWindow(t *testing.T) {
	p := newDriverPeer(t)
	d := newDriver(t, p, p.drive)
	link, err := d.Link(t.Context(), &model.Object{Path: "/hello.txt", Size: 11}, model.LinkArgs{})
	if err != nil {
		t.Fatal(err)
	}
	before := p.count()
	rc, err := link.RangeReader.RangeRead(t.Context(), http_range.Range{Length: -1})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "hello bytes" {
		t.Fatalf("full native read = %q, %v", body, err)
	}
	records := p.all()[before:]
	if len(records) != 1 || records[0].Range != "" {
		t.Fatalf("full native read sent a Range header: %+v", records)
	}
	before = p.count()
	rc, err = link.RangeReader.RangeRead(t.Context(), http_range.Range{Start: 3, Length: 0})
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || len(body) != 0 || p.count() != before {
		t.Fatalf("empty window read peer data: %q, %v", body, err)
	}
	for _, hr := range []http_range.Range{
		{Start: -1, Length: -1},
		{Start: math.MaxInt64, Length: 2},
	} {
		if _, err := link.RangeReader.RangeRead(t.Context(), hr); err == nil {
			t.Errorf("invalid read window %+v was accepted", hr)
		}
	}
	if p.count() != before {
		t.Fatal("invalid windows contacted the peer")
	}
}
