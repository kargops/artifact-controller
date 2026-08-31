// Package ami implements the store driver for EC2 machine images. An AMI is
// the archetype of an expensive artifact that is cheap to check and entirely
// derivable from its inputs — a source image plus a provisioner — which is
// what makes it worth reconciling rather than rebuilding on a schedule.
//
// It speaks the EC2 Query API directly rather than importing the EC2 SDK.
// That SDK is generated code for hundreds of operations and costs roughly
// 2.2GB of build memory; this driver uses three of them. The Query protocol
// has been stable since 2016 and the request shapes here are trivial.
package ami

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/awsauth"
	"github.com/kargops/artifact-controller/internal/store"
)

// ec2APIVersion pins the Query API contract. EC2 keeps old versions working
// indefinitely, so this is a stability guarantee rather than a maintenance
// burden.
const ec2APIVersion = "2016-11-15"

const maxBody = 1 << 20

// Register wires the ami driver into a registry.
func Register(reg *store.Registry) {
	reg.Register("ami", factory)
}

func factory(_ context.Context, class *artifactsv1.ArtifactClass) (store.Driver, error) {
	cfg := class.Spec.Store.AMI
	if cfg == nil {
		return nil, fmt.Errorf("class %s: store.driver is ami but store.ami is not set", class.Name)
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("class %s: store.ami.region is required", class.Name)
	}
	return &driver{
		endpoint:        fmt.Sprintf("https://ec2.%s.amazonaws.com/", cfg.Region),
		region:          cfg.Region,
		owner:           ownerOr(cfg.Owner),
		deleteSnapshots: cfg.DeleteSnapshots,
		client:          &http.Client{Timeout: 30 * time.Second},
		sign:            awsauth.Sign,
	}, nil
}

type driver struct {
	endpoint        string
	region          string
	owner           string
	deleteSnapshots bool
	client          *http.Client
	// sign is injectable so tests can exercise the driver without credentials.
	sign func(ctx context.Context, req *http.Request, region, service string) error
}

func ownerOr(v string) string {
	if v != "" {
		return v
	}
	return "self"
}

type ec2Image struct {
	ImageID string `xml:"imageId"`
	Name    string `xml:"name"`
	State   string `xml:"imageState"`
	Tags    []struct {
		Key   string `xml:"key"`
		Value string `xml:"value"`
	} `xml:"tagSet>item"`
	BlockDevices []struct {
		SnapshotID string `xml:"ebs>snapshotId"`
	} `xml:"blockDeviceMapping>item"`
}

type describeImagesResponse struct {
	Images []ec2Image `xml:"imagesSet>item"`
}

type ec2Error struct {
	Errors []struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Errors>Error"`
}

func (d *driver) call(ctx context.Context, params url.Values, out interface{}) error {
	params.Set("Version", ec2APIVersion)
	body := params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	if err := d.sign(ctx, req, d.region, "ec2"); err != nil {
		return err
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("ec2 %s: %w", params.Get("Action"), err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("read ec2 response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// EC2 reports failures as XML; surfacing its code and message beats a
		// bare status, since InvalidAMIID.Unavailable and UnauthorizedOperation
		// call for very different responses.
		var e ec2Error
		if xml.Unmarshal(raw, &e) == nil && len(e.Errors) > 0 {
			return fmt.Errorf("ec2 %s: %s: %s", params.Get("Action"), e.Errors[0].Code, e.Errors[0].Message)
		}
		return fmt.Errorf("ec2 %s returned %d", params.Get("Action"), resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := xml.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode ec2 response: %w", err)
	}
	return nil
}

func (d *driver) describe(ctx context.Context, key string) (*ec2Image, error) {
	params := url.Values{}
	params.Set("Action", "DescribeImages")
	params.Set("Owner.1", d.owner)
	params.Set("Filter.1.Name", "name")
	params.Set("Filter.1.Value.1", key)

	var out describeImagesResponse
	if err := d.call(ctx, params, &out); err != nil {
		return nil, err
	}
	if len(out.Images) == 0 {
		return nil, nil
	}
	return &out.Images[0], nil
}

// Observe finds the image by name. AMI names are unique per account and
// region, which is what lets the rendered key address exactly one image.
func (d *driver) Observe(ctx context.Context, key string) (store.Observation, error) {
	img, err := d.describe(ctx, key)
	if err != nil || img == nil {
		return store.Observation{}, err
	}

	obs := store.Observation{
		Exists: true,
		// The image id is the closest thing to a content identity an AMI has:
		// a rebuild registers a new one, so a changed id means the image
		// behind this key was replaced.
		Digest:   img.ImageID,
		Metadata: map[string]string{},
	}
	for _, t := range img.Tags {
		obs.Metadata[strings.ToLower(t.Key)] = t.Value
	}

	// An image mid-registration cannot be launched; reporting it as present
	// would let an Artifact go Ready before anything could use it.
	if img.State != "available" {
		obs.Exists = false
	}
	return obs, nil
}

// Delete deregisters the image. Snapshots are left alone unless the class asks
// otherwise: deregistering is recoverable while the snapshots still hold the
// data, and deleting them is not.
func (d *driver) Delete(ctx context.Context, key string) error {
	img, err := d.describe(ctx, key)
	if err != nil {
		return err
	}
	if img == nil {
		return nil // already gone
	}

	// Collect snapshot ids first: after deregistering, the block device
	// mappings are no longer retrievable.
	var snapshots []string
	if d.deleteSnapshots {
		for _, bdm := range img.BlockDevices {
			if bdm.SnapshotID != "" {
				snapshots = append(snapshots, bdm.SnapshotID)
			}
		}
	}

	dereg := url.Values{}
	dereg.Set("Action", "DeregisterImage")
	dereg.Set("ImageId", img.ImageID)
	if err := d.call(ctx, dereg, nil); err != nil {
		return err
	}

	for _, id := range snapshots {
		del := url.Values{}
		del.Set("Action", "DeleteSnapshot")
		del.Set("SnapshotId", id)
		if err := d.call(ctx, del, nil); err != nil {
			// The image is already deregistered, so the artifact is gone as
			// far as the controller is concerned; a snapshot that will not
			// delete is a cost problem, not a reconcile failure.
			return fmt.Errorf("image %s deregistered but snapshot %s remains: %w", img.ImageID, id, err)
		}
	}
	return nil
}

var _ store.Driver = (*driver)(nil)
