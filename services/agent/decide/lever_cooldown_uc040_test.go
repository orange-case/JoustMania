package decide

import (
	"encoding/json"
	"strings"
	"testing"
)

// The lever_cooldown session token of the decide state (#2641; UC-040 A53 step 4;
// decide-contract.md §1.2).

// UC: UC-040 A53
func TestBuildState_LeverCooldown(t *testing.T) {
	t.Run("UC-040/A53 cooling levers are named in field, wire, session line and digest", func(t *testing.T) {
		c := sampleCtx()
		st := BuildStateWith(c, Annotations{Cooling: []string{IntvAdjustGlobalDifficulty, IntvAdjustVolume}})
		session := strings.SplitN(strings.SplitN(st.Narrative, "\nsession: ", 2)[1], "\n", 2)[0]
		if !strings.HasSuffix(session, " lever_cooldown=adjust_global_difficulty,adjust_volume") {
			t.Errorf("session line lacks the token: %q", session)
		}
		raw, _ := json.Marshal(st)
		if !strings.Contains(string(raw), `"lever_cooldown":["adjust_global_difficulty","adjust_volume"]`) {
			t.Errorf("wire lacks lever_cooldown: %s", raw)
		}
		if st.Digest == BuildState(c).Digest {
			t.Error("digest must change with lever_cooldown")
		}
	})
	t.Run("UC-040/A53 no cooling lever keeps the narrative, wire and digest", func(t *testing.T) {
		c := sampleCtx()
		st, plain := BuildStateWith(c, Annotations{Cooling: nil}), BuildState(c)
		if st.Digest != plain.Digest || st.Narrative != plain.Narrative {
			t.Error("an empty cooldown must not change the state")
		}
		raw, _ := json.Marshal(st)
		if strings.Contains(string(raw), "lever_cooldown") {
			t.Errorf("wire carries an empty lever_cooldown: %s", raw)
		}
	})
}
