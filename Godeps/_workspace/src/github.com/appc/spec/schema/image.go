package schema

import (
	"encoding/json"
	"errors"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema/types"
)

const (
	ACIExtension      = ".aci"
	ImageManifestKind = types.ACKind("ImageManifest")
)

type ImageManifest struct {
	ACKind        types.ACKind       `json:"acKind"`
	ACVersion     types.SemVer       `json:"acVersion"`
	Name          types.ACName       `json:"name"`
	Labels        types.Labels       `json:"labels,omitempty"`
	App           *types.App         `json:"app,omitempty"`
	Annotations   types.Annotations  `json:"annotations,omitempty"`
	Dependencies  types.Dependencies `json:"dependencies,omitempty"`
	PathWhitelist []string           `json:"pathWhitelist,omitempty"`
}

// imageManifest is a model to facilitate extra validation during the
// unmarshalling of the ImageManifest
type imageManifest ImageManifest

func BlankImageManifest() *ImageManifest {
	return &ImageManifest{ACKind: ImageManifestKind, ACVersion: AppContainerVersion}
}

func (im *ImageManifest) UnmarshalJSON(data []byte) error {
	a := imageManifest(*im)
	err := json.Unmarshal(data, &a)
	if err != nil {
		return err
	}
	nim := ImageManifest(a)
	if err := nim.assertValid(); err != nil {
		return err
	}
	*im = nim
	return nil
}

func (im ImageManifest) MarshalJSON() ([]byte, error) {
	if err := im.assertValid(); err != nil {
		return nil, err
	}
	return json.Marshal(imageManifest(im))
}

var imKindError = types.InvalidACKindError(ImageManifestKind)

// assertValid performs extra assertions on an ImageManifest to ensure that
// fields are set appropriately, etc. It is used exclusively when marshalling
// and unmarshalling an ImageManifest. Most field-specific validation is
// performed through the individual types being marshalled; assertValid()
// should only deal with higher-level validation.
func (im *ImageManifest) assertValid() error {
	if im.ACKind != ImageManifestKind {
		return imKindError
	}
	if im.ACVersion.Empty() {
		return errors.New(`acVersion must be set`)
	}
	if im.Name.Empty() {
		return errors.New(`name must be set`)
	}
	return nil
}

func (im *ImageManifest) GetLabel(name string) (val string, ok bool) {
	return im.Labels.Get(name)
}

func (im *ImageManifest) GetAnnotation(name string) (val string, ok bool) {
	return im.Annotations.Get(name)
}
