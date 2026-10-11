package db

import (
	"regexp"
	"testing"
)

// The built-in "delete endpoint path" rule matches the entire tool_input JSON string,
// so the cases are given directly in JSON form, matching what the Interceptor actually
// receives as its subject.
func TestDeleteEndpointPathPattern(t *testing.T) {
	re := regexp.MustCompile(deleteEndpointPathPattern)

	hit := []string{
		`{"command":"curl -s 'http://t.com/api/user/delete?id=1'"}`,    // GET hits a delete endpoint
		`{"command":"curl -X POST http://t.com/admin/delete -d id=1"}`, // POST hits a delete endpoint
		`{"command":"curl 'http://t.com/api/deleteAll'"}`,
		`{"command":"curl 'http://t.com/api/delete_user?id=1'"}`,
		`{"command":"curl 'http://t.com/api/delete-user?id=1'"}`,
		`{"url":"http://t.com/api/remove?id=1"}`,
		`{"command":"curl http://t.com/files/unlink/3"}`,
		`{"command":"curl http://t.com/api/del?id=2"}`,
		`{"command":"curl -X POST http://t/v1/erase"}`,
		`{"command":"curl http://t/admin/destroyAll"}`, // the v1 path rule did not allow a suffix; add it here
	}
	for _, s := range hit {
		if !re.MatchString(s) {
			t.Errorf("should have matched but was allowed: %s", s)
		}
	}

	// The verb must be followed by a separator, to avoid blocking read-only paths
	// like /delivery and /details.
	miss := []string{
		`{"command":"curl 'http://t.com/api/delivery?id=1'"}`,
		`{"command":"curl 'http://t.com/order/details'"}`,
		`{"command":"curl 'http://t.com/api/delta/sync'"}`,
		`{"command":"curl 'http://t.com/user/delegate'"}`,
		`{"command":"curl 'http://delete.example.com/'"}`, // the delete verb appears in the domain rather than the path
		`{"command":"curl 'http://t.com/remote/status'"}`,
		`{"command":"nmap -p80 10.0.0.1"}`,
	}
	for _, s := range miss {
		if re.MatchString(s) {
			t.Errorf("false positive: %s", s)
		}
	}
}
