package middleware

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/onetodone/cinema-api/internal/platform/metrics"
)

func TestInstrumentMeasuresByRoutePatternAndStatus(t *testing.T) {
	t.Parallel()

	m := metrics.New(prometheus.NewRegistry())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	// As in the router: Recover inside Instrument, so a recovered panic is measured with its 500.
	h := Chain(mux, Instrument(m), Recover(slog.New(slog.DiscardHandler)))

	for _, path := range []string{"/items/1", "/items/2", "/boom", "/nowhere"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	}

	for _, tt := range []struct {
		route, code string
		want        int
	}{
		{route: "GET /items/{id}", code: "418", want: 2}, // the pattern, never the path: ids do not create series
		{route: "GET /boom", code: "500", want: 1},
		{route: metrics.RouteUnmatched, code: "404", want: 1},
	} {
		if got := histogramCount(t, m, tt.route, tt.code); got != tt.want {
			t.Errorf("requests measured for %s %s = %d, want %d", tt.route, tt.code, got, tt.want)
		}
	}
	if n := testutil.CollectAndCount(m.HTTPRequestDuration); n != 3 {
		t.Errorf("%d series, want 3", n)
	}
}

// histogramCount returns how many observations the duration histogram has for a route and status code.
func histogramCount(t *testing.T, m *metrics.Metrics, route, code string) int {
	t.Helper()
	families, err := prometheus.Gatherers{gathererOf(m.HTTPRequestDuration)}.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, series := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range series.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["route"] == route && labels["code"] == code {
				return int(series.GetHistogram().GetSampleCount())
			}
		}
	}
	return 0
}

// gathererOf registers one collector with a registry of its own.
func gathererOf(c prometheus.Collector) prometheus.Gatherer {
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	return reg
}
