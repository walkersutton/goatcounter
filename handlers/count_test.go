// Copyright © 2019 Martin Tournoij – This file is part of GoatCounter and
// published under the terms of a slightly modified EUPL v1.2 license, which can
// be found in the LICENSE file or at https://license.goatcounter.com

package handlers

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"

	"zgo.at/goatcounter"
	"zgo.at/goatcounter/gctest"
	"zgo.at/isbot"
	"zgo.at/zdb"
	"zgo.at/zstd/zcrypto"
	"zgo.at/zstd/zint"
	"zgo.at/zstd/zjson"
	"zgo.at/zstd/ztest"
	"zgo.at/zstd/ztime"
)

func TestBackendCount(t *testing.T) {
	ztime.SetNow(t, "2019-06-18 14:42:00")

	tests := []struct {
		name     string
		query    url.Values
		set      func(r *http.Request)
		wantCode int
		hit      goatcounter.Hit
	}{
		{"no path", url.Values{}, nil, 400, goatcounter.Hit{}},
		{"invalid size", url.Values{"p": {"/x"}, "s": {"xxx"}}, nil, 400, goatcounter.Hit{}},

		{"only path", url.Values{"p": {"/foo.html"}}, nil, 200, goatcounter.Hit{
			Path: "/foo.html",
		}},

		{"add slash", url.Values{"p": {"foo.html"}}, nil, 200, goatcounter.Hit{
			Path: "/foo.html",
		}},

		{"event", url.Values{"p": {"foo.html"}, "e": {"true"}}, nil, 200, goatcounter.Hit{
			Path:  "foo.html",
			Event: true,
		}},

		{"params", url.Values{"p": {"/foo.html?a=b&c=d"}}, nil, 200, goatcounter.Hit{
			Path: "/foo.html?a=b&c=d",
		}},

		{"ref", url.Values{"p": {"/foo.html"}, "r": {"https://example.com"}}, nil, 200, goatcounter.Hit{
			Path:      "/foo.html",
			Ref:       "example.com",
			RefScheme: ztest.SP("h"),
		}},

		{"str ref", url.Values{"p": {"/foo.html"}, "r": {"example"}}, nil, 200, goatcounter.Hit{
			Path:      "/foo.html",
			Ref:       "example",
			RefScheme: ztest.SP("o"),
		}},

		{"ref params", url.Values{"p": {"/foo.html"}, "r": {"https://example.com?p=x"}}, nil, 200, goatcounter.Hit{
			Path:      "/foo.html",
			Ref:       "example.com",
			RefScheme: ztest.SP("h"),
		}},

		{"full", url.Values{"p": {"/foo.html"}, "t": {"XX"}, "r": {"https://example.com?p=x"}, "s": {"40,50,1"}}, nil, 200, goatcounter.Hit{
			Path:      "/foo.html",
			Title:     "XX",
			Ref:       "example.com",
			RefScheme: ztest.SP("h"),
			Size:      goatcounter.Floats{40, 50, 1},
		}},

		{"campaign", url.Values{"p": {"/foo.html"}, "q": {"ref=XXX"}}, nil, 200, goatcounter.Hit{
			Path:      "/foo.html",
			Ref:       "XXX",
			RefScheme: ztest.SP("c"),
		}},
		{"campaign_override", url.Values{"p": {"/foo.html?ref=AAA"}, "q": {"ref=XXX"}}, nil, 200, goatcounter.Hit{
			Path:      "/foo.html",
			Ref:       "XXX",
			RefScheme: ztest.SP("c"),
		}},

		{"bot", url.Values{"p": {"/a"}, "b": {"150"}}, nil, 200, goatcounter.Hit{
			Path: "/a",
			Bot:  150,
		}},
		{"googlebot", url.Values{"p": {"/a"}, "b": {"150"}}, func(r *http.Request) {
			r.Header.Set("User-Agent", "GoogleBot/1.0")
		}, 200, goatcounter.Hit{
			Path:            "/a",
			Bot:             int(isbot.BotShort),
			UserAgentHeader: "GoogleBot/1.0",
		}},

		{"bot", url.Values{"p": {"/a"}, "b": {"100"}}, nil, 400, goatcounter.Hit{}},

		{"post", url.Values{"p": {"/foo.html"}}, func(r *http.Request) {
			r.Method = "POST"
		}, 200, goatcounter.Hit{
			Path: "/foo.html",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := gctest.DB(t)

			site := goatcounter.Site{
				CreatedAt: time.Date(2019, 01, 01, 0, 0, 0, 0, time.UTC),
			}
			ctx = gctest.Site(ctx, t, &site, nil)

			r, rr := newTest(ctx, "GET", "/count?"+tt.query.Encode(), nil)
			r.Host = site.Code + "." + goatcounter.Config(ctx).Domain
			if tt.set != nil {
				tt.set(r)
			}
			login(t, r)

			newBackend(zdb.MustGetDB(ctx)).ServeHTTP(rr, r)
			if h := rr.Header().Get("X-Goatcounter"); h != "" {
				t.Logf("X-Goatcounter: %s", h)
			}
			ztest.Code(t, rr, tt.wantCode)

			if tt.wantCode >= 400 {
				return
			}

			_, err := goatcounter.Memstore.Persist(ctx)
			if err != nil {
				t.Fatal(err)
			}

			var hits goatcounter.Hits
			err = hits.TestList(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) != 1 {
				t.Fatalf("len(hits) = %d: %#v", len(hits), hits)
			}

			h := hits[0]
			err = h.Validate(ctx, false)
			if err != nil {
				t.Errorf("Validate failed after get: %s", err)
			}

			tt.hit.ID = h.ID
			tt.hit.Site = h.Site
			tt.hit.CreatedAt = ztime.Now()
			tt.hit.Session = goatcounter.TestSeqSession // Should all be the same session.
			if tt.hit.UserAgentHeader == "" {
				tt.hit.UserAgentHeader = "GoatCounter test runner/1.0"
			}
			h.CreatedAt = h.CreatedAt.In(time.UTC)
			if d := ztest.Diff(string(zjson.MustMarshal(h)), string(zjson.MustMarshal(tt.hit))); d != "" {
				t.Error(d)
			}
		})
	}
}

func TestBackendCountSessions(t *testing.T) {
	now := time.Date(2019, 6, 18, 14, 42, 0, 0, time.UTC)
	ztime.Now = func() time.Time { return now }
	defer func() { ztime.Now = func() time.Time { return time.Now().UTC() } }()

	ctx := gctest.DB(t)

	ctx1 := gctest.Site(ctx, t, &goatcounter.Site{
		CreatedAt: time.Date(2019, 01, 01, 0, 0, 0, 0, time.UTC),
	}, nil)
	ctx2 := gctest.Site(ctx, t, &goatcounter.Site{
		CreatedAt: time.Date(2019, 01, 01, 0, 0, 0, 0, time.UTC),
	}, nil)

	send := func(ctx context.Context, ua string) {
		site := Site(ctx)
		query := url.Values{"p": {"/" + zcrypto.Secret64()}}

		r, rr := newTest(ctx, "GET", "/count?"+query.Encode(), nil)
		r.Host = site.Code + "." + goatcounter.Config(ctx).Domain
		r.Header.Set("User-Agent", ua)
		newBackend(zdb.MustGetDB(ctx)).ServeHTTP(rr, r)
		if h := rr.Header().Get("X-Goatcounter"); h != "" {
			t.Logf("X-Goatcounter: %s", h)
		}
		ztest.Code(t, rr, 200)

		_, err := goatcounter.Memstore.Persist(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}

	checkHits := func(ctx context.Context, n int) []goatcounter.Hit {
		var hits goatcounter.Hits
		err := hits.TestList(ctx, true)
		if err != nil {
			t.Fatal(err)
		}

		if len(hits) != n {
			t.Errorf("len(hits) = %d; wanted %d", len(hits), n)
			for _, h := range hits {
				t.Logf("ID: %d; Site: %d; Session: %d\n", h.ID, h.Site, h.Session)
			}
			t.Fatal()
		}

		for _, h := range hits {
			err := h.Validate(ctx, false)
			if err != nil {
				t.Errorf("Validate failed after get: %s", err)
			}
		}
		return hits
	}

	checkSess := func(hits goatcounter.Hits, wantInt []int) {
		var got []zint.Uint128
		for _, h := range hits {
			got = append(got, h.Session)
			if !h.FirstVisit {
				t.Errorf("FirstVisit is false for %v", h)
			}
		}

		first := zint.Uint128{goatcounter.TestSession[0], goatcounter.TestSession[1] + 1}
		want := make([]zint.Uint128, len(wantInt))
		for i := range wantInt {
			want[i] = first
			want[i][1] += uint64(wantInt[i])
		}

		// TODO: test in order.
		sort.Slice(want, func(i, j int) bool { return want[i][1] < want[j][1] })
		var w string
		for _, ww := range want {
			w += ww.Format(16) + " "
		}

		sort.Slice(got, func(i, j int) bool { return got[i][1] < got[j][1] })
		var g string
		for _, gg := range got {
			g += gg.Format(16) + " "
		}

		if w != g {
			t.Errorf("wrong session\nwant: %s\ngot:  %s", w, g)
		}
	}

	rotate := func(ctx context.Context) {
		now = now.Add(12 * time.Hour)
		oldCur, _ := goatcounter.Memstore.GetSalt()

		goatcounter.Memstore.RefreshSalt()

		_, prev := goatcounter.Memstore.GetSalt()
		if string(prev) != string(oldCur) {
			t.Fatalf("salts not cycled?\noldCur: %s\nprev:   %s\n", string(oldCur), string(prev))
		}
	}

	// Ensure salts aren't cycled before they should.
	beforeCur, beforePrev := goatcounter.Memstore.GetSalt()
	now = now.Add(1 * time.Hour)
	goatcounter.Memstore.RefreshSalt()
	afterCur, afterPrev := goatcounter.Memstore.GetSalt()

	before := string(beforeCur) + " → " + string(beforePrev)
	after := string(afterCur) + " → " + string(afterPrev)
	if before != after {
		t.Fatalf("salts cycled too soon\nbefore: %s\nafter: %s", before, after)
	}

	send(ctx1, "test")
	send(ctx1, "test")
	send(ctx1, "other")
	send(ctx2, "test")
	send(ctx2, "test")
	send(ctx1, "test")
	send(ctx1, "other")

	hits1 := checkHits(ctx1, 5)
	hits2 := checkHits(ctx2, 2)

	want := []int{1, 1, 2, 3, 3, 1, 2}
	checkSess(append(hits1, hits2...), want)

	// Rotate, should still use the same sessions.
	rotate(ctx1)
	send(ctx1, "test")
	send(ctx2, "test")
	hits1 = checkHits(ctx1, 6)
	hits2 = checkHits(ctx2, 3)
	want = []int{1, 1, 2, 3, 3, 1, 2, 1, 3}
	checkSess(append(hits1, hits2...), want)

	// Rotate again, should use new sessions from now on.
	rotate(ctx1)
	send(ctx1, "test")
	send(ctx2, "test")
	hits1 = checkHits(ctx1, 7)
	hits2 = checkHits(ctx2, 4)
	want = []int{1, 1, 2, 3, 3, 1, 2, 1, 3, 4, 5}
	checkSess(append(hits1, hits2...), want)
}
