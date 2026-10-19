package templates

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestChartReadoutOnlyWhilePressed: a chart's readout (crosshair, dot, time and value) shows
// while the primary button or a finger is down on the chart, follows the pointer while it
// stays down, and goes away on release. Moving over a chart without pressing shows nothing,
// and neither does the right button, which opens the context menu.
func TestChartReadoutOnlyWhilePressed(t *testing.T) {
	node := needNode(t)
	src, err := os.ReadFile("../static/chart.js")
	if err != nil {
		t.Fatal(err)
	}
	const harness = `
const handlers = {};
class Element {}
class El extends Element {
  constructor() { super(); this.style = {}; this.hidden = true; this.textContent = ""; this.offsetWidth = 40; }
}
const hover = new El(), cross = new El(), dot = new El(), tip = new El(), tipT = new El(), tipV = new El();
const plot = new El();
plot.getBoundingClientRect = () => ({ left: 0, right: 100, width: 100, height: 50 });
const parts = { ".chart__plot": plot, ".chart__hover": hover, ".chart__cross": cross, ".chart__dot": dot,
  ".chart__tip": tip, ".chart__tip-t": tipT, ".chart__tip .fig-v": tipV };
const wrap = new El();
wrap.dataset = { chart: JSON.stringify([{ t: "Mon", v: "$1", y: 0.5 }, { t: "Tue", v: "$2", y: 0.25 }, { t: "Wed", v: "$3", y: 0 }]) };
wrap.querySelector = hover.querySelector = q => parts[q] || null;
const onChart = new El();
onChart.closest = () => wrap;
const document = {
  addEventListener: (type, fn) => { handlers[type] = fn; },
  documentElement: { addEventListener: (type, fn) => { handlers["page:" + type] = fn; } },
};
new Function("document", "Element", process.argv[1])(document, Element);
const fire = (type, clientX, buttons) => (handlers[type] || (() => {}))({ target: onChart, clientX, buttons });
const seen = () => (hover.hidden ? "nothing" : tipT.textContent + " " + tipV.textContent);
const log = [];
fire("pointermove", 50, 0); log.push(seen());   // over the chart, no button
fire("pointerdown", 0, 1); log.push(seen());    // press at the left edge
fire("pointermove", 100, 1); log.push(seen());  // drag right, still pressed
fire("pointerup", 100, 0); log.push(seen());    // release
fire("pointermove", 50, 0); log.push(seen());   // over the chart again, no button
fire("pointerdown", 50, 2); log.push(seen());   // right button
console.log(JSON.stringify(log));
`
	out, err := exec.Command(node, "-e", harness, string(src)).CombinedOutput()
	if err != nil {
		t.Fatalf("chart.js failed: %v\n%s", err, out)
	}
	want := `["nothing","Mon $1","Wed $3","nothing","nothing","nothing"]`
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("readout after move, press, drag, release, move, right press:\n got %s\nwant %s", got, want)
	}
}
