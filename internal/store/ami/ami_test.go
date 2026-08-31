package ami

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestDriver points the driver at a fake EC2 endpoint and stubs signing, so
// the tests need neither credentials nor network.
func newTestDriver(endpoint string, deleteSnapshots bool) *driver {
	return &driver{
		endpoint:        endpoint,
		region:          "eu-central-1",
		owner:           "self",
		deleteSnapshots: deleteSnapshots,
		client:          &http.Client{Timeout: 5 * time.Second},
		sign:            func(context.Context, *http.Request, string, string) error { return nil },
	}
}

// ec2Fake answers the Query API the way EC2 does: form-encoded requests in,
// XML out, with Action selecting the operation.
type ec2Fake struct {
	imagesByName map[string]string // name -> XML <item> body
	actions      []string
	params       []url.Values
	failAction   string
}

func (f *ec2Fake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		action := r.PostForm.Get("Action")
		f.actions = append(f.actions, action)
		f.params = append(f.params, r.PostForm)

		if action == f.failAction {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`<Response><Errors><Error>` +
				`<Code>InvalidSnapshot.InUse</Code><Message>snapshot is in use</Message>` +
				`</Error></Errors></Response>`))
			return
		}

		switch action {
		case "DescribeImages":
			name := r.PostForm.Get("Filter.1.Value.1")
			item := f.imagesByName[name]
			_, _ = w.Write([]byte(`<DescribeImagesResponse><imagesSet>` + item + `</imagesSet></DescribeImagesResponse>`))
		case "DeregisterImage":
			_, _ = w.Write([]byte(`<DeregisterImageResponse><return>true</return></DeregisterImageResponse>`))
		case "DeleteSnapshot":
			_, _ = w.Write([]byte(`<DeleteSnapshotResponse><return>true</return></DeleteSnapshotResponse>`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}
}

func imageXML(id, name, state string, tags map[string]string, snapshots ...string) string {
	var b strings.Builder
	b.WriteString("<item><imageId>" + id + "</imageId><name>" + name + "</name>")
	b.WriteString("<imageState>" + state + "</imageState><tagSet>")
	for k, v := range tags {
		b.WriteString("<item><key>" + k + "</key><value>" + v + "</value></item>")
	}
	b.WriteString("</tagSet><blockDeviceMapping>")
	for _, s := range snapshots {
		b.WriteString("<item><ebs><snapshotId>" + s + "</snapshotId></ebs></item>")
	}
	b.WriteString("</blockDeviceMapping></item>")
	return b.String()
}

func TestObserveReportsImageIdAndTags(t *testing.T) {
	f := &ec2Fake{imagesByName: map[string]string{
		"base-2026-08-a1b2c3": imageXML("ami-0123", "base-2026-08-a1b2c3", "available",
			map[string]string{"artifact-spec-hash": "sha256:cafe", "Name": "base"}),
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	d := newTestDriver(srv.URL, false)

	obs, err := d.Observe(context.Background(), "base-2026-08-a1b2c3")
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Exists {
		t.Fatal("expected Exists")
	}
	if obs.Digest != "ami-0123" {
		t.Errorf("digest = %q, want the image id", obs.Digest)
	}
	if got := obs.Metadata["artifact-spec-hash"]; got != "sha256:cafe" {
		t.Errorf("stamp = %q", got)
	}
	if _, ok := obs.Metadata["name"]; !ok {
		t.Error("tag keys should be lowercased")
	}
	// The lookup must be server-side, by name, scoped to the owner.
	p := f.params[0]
	if p.Get("Filter.1.Name") != "name" || p.Get("Owner.1") != "self" {
		t.Errorf("unexpected query params: %v", p)
	}
}

func TestObserveMissingIsNotAnError(t *testing.T) {
	f := &ec2Fake{imagesByName: map[string]string{}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	obs, err := newTestDriver(srv.URL, false).Observe(context.Background(), "nothing-here")
	if err != nil {
		t.Fatalf("absent image must not error: %v", err)
	}
	if obs.Exists {
		t.Error("expected absent")
	}
}

// An image still registering cannot be launched, so reporting it present would
// let an Artifact go Ready before it is usable.
func TestPendingImageIsNotYetPresent(t *testing.T) {
	f := &ec2Fake{imagesByName: map[string]string{
		"half-baked": imageXML("ami-9999", "half-baked", "pending", nil),
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	obs, err := newTestDriver(srv.URL, false).Observe(context.Background(), "half-baked")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Exists {
		t.Error("a pending image must not count as present")
	}
}

func TestDeleteDeregistersAndLeavesSnapshotsByDefault(t *testing.T) {
	f := &ec2Fake{imagesByName: map[string]string{
		"doomed": imageXML("ami-dead", "doomed", "available", nil, "snap-1", "snap-2"),
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	d := newTestDriver(srv.URL, false)

	if err := d.Delete(context.Background(), "doomed"); err != nil {
		t.Fatal(err)
	}
	if !contains(f.actions, "DeregisterImage") {
		t.Errorf("actions = %v", f.actions)
	}
	if contains(f.actions, "DeleteSnapshot") {
		t.Error("snapshots must survive unless the class asks otherwise")
	}

	if err := d.Delete(context.Background(), "never-existed"); err != nil {
		t.Errorf("absent image delete must be a no-op: %v", err)
	}
}

func TestDeleteSnapshotsWhenAsked(t *testing.T) {
	f := &ec2Fake{imagesByName: map[string]string{
		"doomed": imageXML("ami-dead", "doomed", "available", nil, "snap-1", "snap-2"),
	}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	if err := newTestDriver(srv.URL, true).Delete(context.Background(), "doomed"); err != nil {
		t.Fatal(err)
	}
	var deletes int
	for _, a := range f.actions {
		if a == "DeleteSnapshot" {
			deletes++
		}
	}
	if deletes != 2 {
		t.Errorf("DeleteSnapshot called %d times, want 2", deletes)
	}
}

// A snapshot that will not delete is a cost problem, but the image is already
// deregistered — the error must say so rather than implying nothing happened.
func TestSnapshotFailureReportsThatTheImageIsAlreadyGone(t *testing.T) {
	f := &ec2Fake{
		imagesByName: map[string]string{
			"doomed": imageXML("ami-dead", "doomed", "available", nil, "snap-1"),
		},
		failAction: "DeleteSnapshot",
	}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	err := newTestDriver(srv.URL, true).Delete(context.Background(), "doomed")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !contains(f.actions, "DeregisterImage") {
		t.Error("the image should still have been deregistered")
	}
	for _, want := range []string{"deregistered", "snap-1", "InvalidSnapshot.InUse"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// EC2 reports failures as XML; a bare status code would lose the distinction
// between an unavailable image and a permissions problem.
func TestApiErrorsSurfaceCodeAndMessage(t *testing.T) {
	f := &ec2Fake{imagesByName: map[string]string{}, failAction: "DescribeImages"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	_, err := newTestDriver(srv.URL, false).Observe(context.Background(), "x")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "InvalidSnapshot.InUse") || !strings.Contains(err.Error(), "in use") {
		t.Errorf("error should carry the EC2 code and message: %v", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
