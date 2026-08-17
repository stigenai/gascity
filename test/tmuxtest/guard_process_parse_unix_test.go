//go:build !windows

package tmuxtest

import (
	"reflect"
	"testing"
)

func TestParseTestProcessGroupMembersReportsExactSortedPIDs(t *testing.T) {
	input := "" +
		"  91  700\n" +
		"broken\n" +
		"  42  701\n" +
		"  17  700\n" +
		"   1  700\n" +
		"  bad 700\n"
	if got, want := parseTestProcessGroupMembers(input, 700), []int{17, 91}; !reflect.DeepEqual(got, want) {
		t.Fatalf("members = %v, want %v", got, want)
	}
}
