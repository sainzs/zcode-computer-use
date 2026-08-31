package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OCR via Apple Vision: `ocr` reads text out of the CURRENT screenshot — the
// fallback for apps whose AX tree is empty or unusable (Electron, canvases,
// games). Boxes come back in screenshot pixels, so `click x/y` works on them
// directly. Read-only: it reads the PNG on disk and never touches the GUI,
// so element indices stay valid.

// lines cap: a dense capture can recognize hundreds of regions; past this
// the payload is noise and the agent should narrow with filter
const ocrLinesMax = 200

type ocrPayload struct {
	Result string   `json:"result"`
	Lines  []string `json:"lines"`
	Hint   string   `json:"hint"`
}

// The recipe is proven verbatim on this machine (prewalk, 2026-08): the `$()`
// options literal and `alloc.init` without a completion handler are both
// load-bearing — initWithURLOptions needs the empty NSDictionary (nil trips
// the JXA bridge) and the completion-handler variant never returns under
// JXA. %PATH% is a JSON-marshaled string, i.e. a quoted JS literal.
const ocrJxaSrc = `
ObjC.import('Foundation');
ObjC.import('Vision');
const url = $.NSURL.fileURLWithPath(%PATH%);
const handler = $.VNImageRequestHandler.alloc.initWithURLOptions(url, $());
const request = $.VNRecognizeTextRequest.alloc.init;
request.recognitionLevel = $.VNRequestTextRecognitionLevelAccurate;
const err = Ref();
if (!handler.performRequestsError($.NSArray.arrayWithObject(request), err)) throw 'vision failed';
const results = request.results;
const out = [];
for (let i = 0; i < results.count; i++) {
  const obs = results.objectAtIndex(i);
  const txt = ObjC.unwrap(obs.topCandidates(1).objectAtIndex(0).string);
  const bb = obs.boundingBox;
  out.push(JSON.stringify([txt, bb.origin.x, bb.origin.y, bb.size.width, bb.size.height]));
}
out.join('\n');
`

// ocrBoxToPixels converts a Vision bounding box — normalized 0..1 with a
// BOTTOM-LEFT origin — into screenshot pixels (TOP-LEFT origin), rounded to
// whole pixels. Pure function, golden-tested.
func ocrBoxToPixels(x, y, w, h float64, imgW, imgH int) (int, int, int, int) {
	px := int(x*float64(imgW) + 0.5)
	py := int((1-y-h)*float64(imgH) + 0.5)
	pw := int(w*float64(imgW) + 0.5)
	ph := int(h*float64(imgH) + 0.5)
	return px, py, pw, ph
}

func toolOCR(st *serverState, a args) (any, *ToolError) {
	filter, terr := argStr(a, "filter", false)
	if terr != nil {
		return nil, terr
	}
	// the capture may be stale — reading the PNG does not depend on the tree
	if terr := requireState(st, false); terr != nil {
		return nil, terr
	}
	if st.Screenshot == "" {
		return nil, toolErr("no_state", "no screenshot to read",
			"call get_app_state first")
	}
	imgW, imgH, err := pngSize(st.Screenshot)
	if err != nil {
		return nil, toolErr("internal",
			fmt.Sprintf("current screenshot is unreadable: %v", err),
			"call get_app_state to capture a fresh one")
	}
	pathJSON, _ := json.Marshal(st.Screenshot)
	raw, terr := osascript(strings.ReplaceAll(ocrJxaSrc, "%PATH%", string(pathJSON)),
		"JavaScript", 0)
	if terr != nil {
		return nil, terr
	}

	low := strings.ToLower(filter)
	lines := []string{}
	for _, ln := range strings.Split(raw, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		// each line is [text, x, y, w, h]; junk lines are skipped, like
		// parseDump's — Vision never emits them, but don't trust the channel
		var row [5]any
		if json.Unmarshal([]byte(ln), &row) != nil {
			continue
		}
		txt, ok := row[0].(string)
		if !ok {
			continue
		}
		box := [4]float64{}
		for i, v := range row[1:] {
			f, isNum := v.(float64)
			if !isNum {
				ok = false
				break
			}
			box[i] = f
		}
		if !ok {
			continue
		}
		if low != "" && !strings.Contains(strings.ToLower(txt), low) {
			continue
		}
		px, py, pw, ph := ocrBoxToPixels(box[0], box[1], box[2], box[3], imgW, imgH)
		lines = append(lines, fmt.Sprintf("%q @px:%d,%d:%dx%d", txt, px, py, pw, ph))
		if len(lines) >= ocrLinesMax {
			break
		}
	}
	hint := "coordinates are screenshot pixels — click x/y works on the box centers"
	if st.Stale {
		hint = "screenshot predates the last action — get_app_state for " +
			"fresh pixels before clicking these coordinates"
	}
	return ocrPayload{
		Result: fmt.Sprintf("%d text region(s)", len(lines)),
		Lines:  lines,
		Hint:   hint,
	}, nil
}
