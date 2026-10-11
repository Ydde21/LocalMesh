package metrics

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ContentType is the Prometheus text exposition format v0.0.4.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Handler serves GET (and HEAD) with the text exposition of every
// counter plus a fresh snapshot from the gauge provider. Mount it
// wherever the integrator wants — the metrics themselves carry no
// secrets, but the route's authn/z is the integrator's choice.
func (c *Collector) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var b strings.Builder
		b.Grow(4096)
		c.render(&b)
		w.Header().Set("Content-Type", ContentType)
		w.WriteHeader(http.StatusOK)
		// A HEAD request still writes headers with the real length.
		if r.Method == http.MethodGet {
			w.Write([]byte(b.String()))
		}
	})
}

// render writes the full exposition: counters in declaration order,
// series sorted by label encoding, then histograms, then provider
// gauges sorted by name.
func (c *Collector) render(b *strings.Builder) {
	for _, v := range c.vecs {
		writeMeta(b, v.name, "counter", v.help)
		for _, s := range v.snapshot() {
			writeSample(b, v.name, s.labels, strconv.FormatUint(s.value, 10))
		}
	}

	for _, v := range c.hvecs {
		writeMeta(b, v.name, "histogram", v.help)
		for _, s := range v.snapshot() {
			// Prometheus histogram exposition: cumulative _bucket
			// counts ordered by le, then _sum and _count.
			var cum uint64
			for i, bound := range v.buckets {
				cum += s.counts[i]
				ls := append(append([]label(nil), s.labels...), label{"le", formatFloat(bound)})
				writeSample(b, v.name+"_bucket", ls, strconv.FormatUint(cum, 10))
			}
			cum += s.counts[len(v.buckets)]
			ls := append(append([]label(nil), s.labels...), label{"le", "+Inf"})
			writeSample(b, v.name+"_bucket", ls, strconv.FormatUint(cum, 10))
			writeSample(b, v.name+"_sum", s.labels, formatFloat(s.sum))
			writeSample(b, v.name+"_count", s.labels, strconv.FormatUint(s.total, 10))
		}
	}

	gauges := c.gauges()
	sort.Slice(gauges, func(i, j int) bool { return gauges[i].Name < gauges[j].Name })
	seen := map[string]bool{}
	for _, g := range gauges {
		name := g.Name
		if !strings.HasPrefix(name, "localmesh_") {
			name = "localmesh_" + name
		}
		if !validName(name) {
			c.log.Debug("skipping gauge with invalid name", "name", g.Name)
			continue
		}
		// HELP/TYPE are emitted once per name — a provider returning
		// duplicate names gets samples under one family.
		if !seen[name] {
			writeMeta(b, name, "gauge", g.Help)
			seen[name] = true
		}
		ls := make([]label, 0, len(g.Labels))
		for k, v := range g.Labels {
			if validName(k) {
				ls = append(ls, label{k, v})
			}
		}
		sort.Slice(ls, func(i, j int) bool { return ls[i].k < ls[j].k })
		writeSample(b, name, ls, formatFloat(g.Value))
	}
}

func writeMeta(b *strings.Builder, name, typ, help string) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	if help != "" {
		b.WriteByte(' ')
		b.WriteString(help)
	}
	b.WriteString("\n# TYPE ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(typ)
	b.WriteByte('\n')
}

func writeSample(b *strings.Builder, name string, ls []label, val string) {
	b.WriteString(name)
	if len(ls) > 0 {
		b.WriteByte('{')
		for i, l := range ls {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l.k)
			b.WriteString(`="`)
			b.WriteString(escape(l.v))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(val)
	b.WriteByte('\n')
}

// escape applies label-value escaping per the exposition spec:
// backslash, double-quote and newline.
func escape(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// formatFloat renders a gauge value; NaN/Inf use the exposition spellings.
func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "Nan"
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// validName reports whether s is a legal Prometheus metric/label name.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}
