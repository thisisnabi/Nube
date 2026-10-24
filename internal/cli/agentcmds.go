package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/thisisnabi/Nube/internal/api"
)

// cmdApprovals lists requests waiting for (or decided by) a human.
func cmdApprovals(c *client, a args) error {
	q := url.Values{}
	if a.flags["all-namespaces"] == "" {
		q.Set("status", "Pending")
	}
	var out struct {
		Items []api.Approval `json:"items"`
	}
	if err := c.getJSON("/api/v1/approvals?"+q.Encode(), &out); err != nil {
		return err
	}
	if a.flags["output"] == "json" {
		return printJSON(out)
	}
	if len(out.Items) == 0 {
		fmt.Println("no pending approvals (-A shows decided ones too)")
		return nil
	}
	w := newTab()
	fmt.Fprintln(w, "ID\tSTATUS\tREQUESTER\tWHAT\tWHY\tAGE")
	for _, ap := range out.Items {
		what := ap.Summary
		if ap.Tool != "" {
			what = "tool " + ap.Tool + " " + truncate(ap.Input, 60)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", ap.ID, ap.Status, ap.Requester, what, ap.Reason, age(ap.CreatedAt))
	}
	return w.Flush()
}

// cmdDecide approves or rejects a held request.
func cmdDecide(c *client, a args, action string, rest []string) error {
	if len(rest) != 1 {
		return fmt.Errorf("usage: nube %s APPROVAL_ID [--note TEXT]", action)
	}
	var out api.Approval
	if err := c.postJSON("/api/v1/approvals/"+url.PathEscape(rest[0])+"/"+action, map[string]string{"note": a.flags["note"]}, &out); err != nil {
		return err
	}
	if a.flags["output"] == "json" {
		return printJSON(out)
	}
	fmt.Printf("%s: %s (%s)\n", out.ID, strings.ToLower(out.Status), out.Summary)
	if out.Result != "" {
		fmt.Println("result:", out.Result)
	}
	return nil
}

func cmdAudit(c *client, a args) error {
	q := url.Values{"limit": {firstNonEmpty(a.flags["tail"], "50")}}
	if who := a.flags["name"]; who != "" {
		q.Set("who", who)
	}
	var out struct {
		Items []api.AuditEntry `json:"items"`
	}
	if err := c.getJSON("/api/v1/audit?"+q.Encode(), &out); err != nil {
		return err
	}
	if a.flags["output"] == "json" {
		return printJSON(out)
	}
	w := newTab()
	fmt.Fprintln(w, "AGE\tWHO\tROLE\tSTATUS\tCHANGE\tAPPROVAL")
	for _, e := range out.Items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", age(e.Time), e.Who, e.Role, e.Status, e.Summary, e.Approval)
	}
	return w.Flush()
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
