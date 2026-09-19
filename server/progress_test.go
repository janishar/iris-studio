package server

import "testing"

// What iris actually writes, and what must not be mistaken for it. Every
// line here is copied from the source named beside it, not invented.
func TestProgressIsReadFromWhatIrisPrints(t *testing.T) {
	cases := []struct {
		line     string
		num, den int
		from     string
	}{
		// iris.c/main.c, cli_step_callback: the one-shot CLI, which is what
		// the studio runs when it passes -p and -o.
		{"  Step 2/4 ddddddddssssF", 2, 4, "one-shot, mid-step"},
		{"  Step 1/4 dddddddd", 1, 4, "one-shot, part-way through a step"},
		// pump trims the trailing space iris writes, so the last step of a
		// render arrives with nothing after the total.
		{"  Step 4/4", 4, 4, "one-shot, trailing space trimmed"},
		{"  Step 12/50 d", 12, 50, "one-shot, two digits"},

		// iris.c/iris_cli.c, cli_step_progress: the REPL, which is what
		// interactive mode drives.
		{"[2/4]:ddsssf", 2, 4, "REPL, mid-step"},
		{"[1/4]:", 1, 4, "REPL, step just started"},
		{"[12/50]:d", 12, 50, "REPL, two digits"},
	}
	for _, c := range cases {
		num, den, ok := stepProgress(c.line)
		if !ok || num != c.num || den != c.den {
			t.Errorf("%s: stepProgress(%q) = %d, %d, %v; want %d, %d, true",
				c.from, c.line, num, den, ok, c.num, c.den)
		}
	}

	never := []struct{ line, why string }{
		// iris.c/iris_sample.c: the timing breakdown, printed after the
		// render. Reading these as progress would wind the bar backwards
		// over a render that has already finished.
		{"  Step 1: 1759.6 ms", "the timing breakdown"},
		{"  Step 4: 945.7 ms", "the timing breakdown"},
		// iris.c/iris_cli.c: !explore counts images, not denoising steps.
		{"  [1/4] Seed: 4242 ", "!explore's per-image counter"},
		// iris.c/main.c and iris_cli.c: the step-image marker --show-steps
		// prints, which stepMarkRe reads and this must not.
		{"[Step 3]", "the --show-steps marker"},
		{"Denoising (d=double block, s=single blocks, F=final):", "the legend"},
		// A prompt echoed back, which is why the one-shot pattern is anchored.
		{"Prompt: a cat on Step 2/4 of a staircase", "a prompt that says Step 2/4"},
		{"Total generation time: 23.8 seconds", "an ordinary line"},
	}
	for _, c := range never {
		if num, den, ok := stepProgress(c.line); ok {
			t.Errorf("%s: stepProgress(%q) = %d, %d, true; want no match", c.why, c.line, num, den)
		}
	}
}
