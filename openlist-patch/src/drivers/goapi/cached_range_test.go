package goapi

import (
	"io"
	"sync"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/goapiclient"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

func TestCachedRangeReaderDoesNotShareMutableHop(t *testing.T) {
	p := newDriverPeer(t)
	d := newDriver(t, p, p.drive)
	link, err := d.Link(goapiclient.WithHop(t.Context(), 1), &model.Object{Path: "/hello.txt", Size: 11}, model.LinkArgs{})
	if err != nil {
		t.Fatal(err)
	}
	read := func(hop int) {
		r, err := link.RangeReader.RangeRead(goapiclient.WithHop(t.Context(), hop), http_range.Range{Length: -1})
		if err != nil {
			t.Error(err)
			return
		}
		body, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || string(body) != "hello bytes" {
			t.Errorf("cached read: %q, %v", body, err)
		}
	}
	before := p.count()
	read(3)
	read(0)
	records := p.all()[before:]
	if len(records) != 2 || records[0].Hop != "4" || records[1].Hop != "2" {
		t.Fatalf("a deeper reader poisoned a later shallower request: %+v", records)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(hop int) {
			defer wg.Done()
			read(hop)
		}(i % 4)
	}
	wg.Wait()
}
