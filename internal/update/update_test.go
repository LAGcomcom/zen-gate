package update

import "testing"

func TestNewerComparesSemverCorrectly(t *testing.T) {
	cases := []struct {
		remote, current string
		want            bool
	}{
		{"1.5.0", "1.4.11", true},  // minor beats patch depth
		{"1.4.11", "1.4.2", true},  // numeric, not string compare ("11" < "2")
		{"1.4.11", "1.4.11", false},
		{"1.4.0", "1.4.11", false},
		{"v1.6.0", "1.5.0", true}, // tag prefix tolerated
		{"", "1.4.11", false},     // unparseable remote never updates
	}
	for _, c := range cases {
		if got := Newer(c.remote, c.current); got != c.want {
			t.Fatalf("Newer(%q, %q) = %v, want %v", c.remote, c.current, got, c.want)
		}
	}
}

func TestCheckAnswersWithoutFeed(t *testing.T) {
	// The pre-fix default: an empty feed must report "no update" without
	// erroring — but the shipped default now points at this repository.
	has, _, _, _, err := Check("", nil)
	if has || err != nil {
		t.Fatalf("empty feed must be a silent no: has=%v err=%v", has, err)
	}
	if FeedURL == "" {
		t.Fatal("FeedURL default must not be empty: the update check would never look anywhere")
	}
}
