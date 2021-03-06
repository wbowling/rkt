package common

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/docker2aci/tarball"
	"strings"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/docker2aci/lib/types"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/docker2aci/lib/util"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/aci"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema"
	appctypes "github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema/types"
)

const (
	defaultTag    = "latest"
	schemaVersion = "0.5.1"
)

func ParseDockerURL(arg string) *types.ParsedDockerURL {
	if arg == "" {
		return nil
	}

	taglessRemote, tag := parseRepositoryTag(arg)
	if tag == "" {
		tag = defaultTag
	}
	indexURL, imageName := SplitReposName(taglessRemote)

	return &types.ParsedDockerURL{
		IndexURL:  indexURL,
		ImageName: imageName,
		Tag:       tag,
	}
}

func GenerateACI(layerData types.DockerImageData, dockerURL *types.ParsedDockerURL, outputDir string, layerFile *os.File, curPwl []string, compress bool) (string, *schema.ImageManifest, error) {
	manifest, err := GenerateManifest(layerData, dockerURL)
	if err != nil {
		return "", nil, fmt.Errorf("error generating the manifest: %v", err)
	}

	imageName := strings.Replace(dockerURL.ImageName, "/", "-", -1)
	aciPath := imageName + "-" + layerData.ID
	if dockerURL.Tag != "" {
		aciPath += "-" + dockerURL.Tag
	}
	if layerData.OS != "" {
		aciPath += "-" + layerData.OS
		if layerData.Architecture != "" {
			aciPath += "-" + layerData.Architecture
		}
	}
	aciPath += ".aci"

	aciPath = path.Join(outputDir, aciPath)
	manifest, err = writeACI(layerFile, *manifest, curPwl, aciPath, compress)
	if err != nil {
		return "", nil, fmt.Errorf("error writing ACI: %v", err)
	}

	if err := ValidateACI(aciPath); err != nil {
		return "", nil, fmt.Errorf("invalid ACI generated: %v", err)
	}

	return aciPath, manifest, nil
}

func ValidateACI(aciPath string) error {
	aciFile, err := os.Open(aciPath)
	if err != nil {
		return err
	}
	defer aciFile.Close()

	reader, err := aci.NewCompressedTarReader(aciFile)
	if err != nil {
		return err
	}

	if err := aci.ValidateArchive(reader); err != nil {
		return err
	}

	return nil
}

func GenerateManifest(layerData types.DockerImageData, dockerURL *types.ParsedDockerURL) (*schema.ImageManifest, error) {
	dockerConfig := layerData.Config
	genManifest := &schema.ImageManifest{}

	appURL := ""
	// omit docker hub index URL in app name
	if dockerURL.IndexURL != defaultIndex {
		appURL = dockerURL.IndexURL + "/"
	}
	appURL += dockerURL.ImageName + "-" + layerData.ID
	appURL, err := appctypes.SanitizeACName(appURL)
	if err != nil {
		return nil, err
	}
	name := appctypes.MustACName(appURL)
	genManifest.Name = *name

	acVersion, err := appctypes.NewSemVer(schemaVersion)
	if err != nil {
		panic("invalid appc spec version")
	}
	genManifest.ACVersion = *acVersion

	genManifest.ACKind = appctypes.ACKind(schema.ImageManifestKind)

	var (
		labels       appctypes.Labels
		parentLabels appctypes.Labels
		annotations  appctypes.Annotations
	)

	layer := appctypes.MustACName("layer")
	labels = append(labels, appctypes.Label{Name: *layer, Value: layerData.ID})

	tag := dockerURL.Tag
	version := appctypes.MustACName("version")
	labels = append(labels, appctypes.Label{Name: *version, Value: tag})

	if layerData.OS != "" {
		os := appctypes.MustACName("os")
		labels = append(labels, appctypes.Label{Name: *os, Value: layerData.OS})
		parentLabels = append(parentLabels, appctypes.Label{Name: *os, Value: layerData.OS})

		if layerData.Architecture != "" {
			arch := appctypes.MustACName("arch")
			parentLabels = append(parentLabels, appctypes.Label{Name: *arch, Value: layerData.Architecture})
		}
	}

	if layerData.Author != "" {
		authorsKey := appctypes.MustACName("authors")
		annotations = append(annotations, appctypes.Annotation{Name: *authorsKey, Value: layerData.Author})
	}
	epoch := time.Unix(0, 0)
	if !layerData.Created.Equal(epoch) {
		createdKey := appctypes.MustACName("created")
		annotations = append(annotations, appctypes.Annotation{Name: *createdKey, Value: layerData.Created.Format(time.RFC3339)})
	}
	if layerData.Comment != "" {
		commentKey := appctypes.MustACName("docker/comment")
		annotations = append(annotations, appctypes.Annotation{Name: *commentKey, Value: layerData.Comment})
	}

	genManifest.Labels = labels
	genManifest.Annotations = annotations

	if dockerConfig != nil {
		exec := getExecCommand(dockerConfig.Entrypoint, dockerConfig.Cmd)
		if exec != nil {
			user, group := parseDockerUser(dockerConfig.User)
			var env appctypes.Environment
			for _, v := range dockerConfig.Env {
				parts := strings.SplitN(v, "=", 2)
				env.Set(parts[0], parts[1])
			}
			app := &appctypes.App{
				Exec:             exec,
				User:             user,
				Group:            group,
				Environment:      env,
				WorkingDirectory: dockerConfig.WorkingDir,
			}

			app.MountPoints, err = convertVolumesToMPs(dockerConfig.Volumes)
			if err != nil {
				return nil, err
			}

			genManifest.App = app
		}
	}

	if layerData.Parent != "" {
		parentAppNameString := dockerURL.IndexURL + "/" + dockerURL.ImageName + "-" + layerData.Parent
		parentAppNameString, err := appctypes.SanitizeACName(parentAppNameString)
		if err != nil {
			return nil, err
		}
		parentAppName := appctypes.MustACName(parentAppNameString)

		genManifest.Dependencies = append(genManifest.Dependencies, appctypes.Dependency{App: *parentAppName, Labels: parentLabels})
	}

	return genManifest, nil
}

func convertVolumesToMPs(dockerVolumes map[string]struct{}) ([]appctypes.MountPoint, error) {
	mps := []appctypes.MountPoint{}
	dup := make(map[string]int)

	for p := range dockerVolumes {
		n := filepath.Join("volume-", p)
		sn, err := appctypes.SanitizeACName(n)
		if err != nil {
			return nil, err
		}

		// check for duplicate names
		if i, ok := dup[sn]; ok {
			dup[sn] = i + 1
			sn = fmt.Sprintf("%s-%d", sn, i)
		} else {
			dup[sn] = 1
		}

		mp := appctypes.MountPoint{
			Name: *appctypes.MustACName(sn),
			Path: p,
		}

		mps = append(mps, mp)
	}

	return mps, nil
}

func writeACI(layer io.ReadSeeker, manifest schema.ImageManifest, curPwl []string, output string, compress bool) (*schema.ImageManifest, error) {
	aciFile, err := os.Create(output)
	if err != nil {
		return nil, fmt.Errorf("error creating ACI file: %v", err)
	}
	defer aciFile.Close()

	var w io.WriteCloser = aciFile
	if compress {
		w = gzip.NewWriter(aciFile)
		defer w.Close()
	}
	trw := tar.NewWriter(w)
	defer trw.Close()

	if err := WriteRootfsDir(trw); err != nil {
		return nil, fmt.Errorf("error writing rootfs entry: %v", err)
	}

	var whiteouts []string
	convWalker := func(t *tarball.TarFile) error {
		name := t.Name()
		if name == "./" {
			return nil
		}
		t.Header.Name = path.Join("rootfs", name)
		absolutePath := strings.TrimPrefix(t.Header.Name, "rootfs")
		if strings.Contains(t.Header.Name, "/.wh.") {
			whiteouts = append(whiteouts, strings.Replace(absolutePath, ".wh.", "", 1))
			return nil
		}
		if t.Header.Typeflag == tar.TypeLink {
			t.Header.Linkname = path.Join("rootfs", t.Linkname())
		}

		if err := trw.WriteHeader(t.Header); err != nil {
			return err
		}
		if _, err := io.Copy(trw, t.TarStream); err != nil {
			return err
		}

		if !util.In(curPwl, absolutePath) {
			curPwl = append(curPwl, absolutePath)
		}

		return nil
	}
	reader, err := aci.NewCompressedTarReader(layer)
	if err == nil {
		// write files in rootfs/
		if err := tarball.Walk(*reader, convWalker); err != nil {
			return nil, err
		}
	} else {
		// ignore errors: empty layers in tars generated by docker save are not
		// valid tar files so we ignore errors trying to open them. Converted
		// ACIs will have the manifest and an empty rootfs directory in any
		// case.
	}
	newPwl := subtractWhiteouts(curPwl, whiteouts)

	manifest.PathWhitelist = newPwl
	if err := WriteManifest(trw, manifest); err != nil {
		return nil, fmt.Errorf("error writing manifest: %v", err)
	}

	return &manifest, nil
}

func getExecCommand(entrypoint []string, cmd []string) appctypes.Exec {
	var command []string
	if entrypoint == nil && cmd == nil {
		return nil
	}
	command = append(entrypoint, cmd...)
	// non-absolute paths are not allowed, fallback to "/bin/sh -c command"
	if len(command) > 0 && !filepath.IsAbs(command[0]) {
		command_prefix := []string{"/bin/sh", "-c"}
		quoted_command := util.Quote(command)
		command = append(command_prefix, strings.Join(quoted_command, " "))
	}
	return command
}

func parseDockerUser(dockerUser string) (string, string) {
	// if the docker user is empty assume root user and group
	if dockerUser == "" {
		return "0", "0"
	}

	dockerUserParts := strings.Split(dockerUser, ":")

	// when only the user is given, the docker spec says that the default and
	// supplementary groups of the user in /etc/passwd should be applied.
	// Assume root group for now in this case.
	if len(dockerUserParts) < 2 {
		return dockerUserParts[0], "0"
	}

	return dockerUserParts[0], dockerUserParts[1]
}

func subtractWhiteouts(pathWhitelist []string, whiteouts []string) []string {
	matchPaths := []string{}
	for _, path := range pathWhitelist {
		// If one of the parent dirs of the current path matches the
		// whiteout then also this path should be removed
		curPath := path
		for curPath != "/" {
			for _, whiteout := range whiteouts {
				if curPath == whiteout {
					matchPaths = append(matchPaths, path)
				}
			}
			curPath = filepath.Dir(curPath)
		}
	}
	for _, matchPath := range matchPaths {
		idx := util.IndexOf(pathWhitelist, matchPath)
		if idx != -1 {
			pathWhitelist = append(pathWhitelist[:idx], pathWhitelist[idx+1:]...)
		}
	}

	return pathWhitelist
}

func WriteManifest(outputWriter *tar.Writer, manifest schema.ImageManifest) error {
	b, err := json.Marshal(manifest)
	if err != nil {
		return err
	}

	hdr := getGenericTarHeader()
	hdr.Name = "manifest"
	hdr.Mode = 0644
	hdr.Size = int64(len(b))
	hdr.Typeflag = tar.TypeReg

	if err := outputWriter.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := outputWriter.Write(b); err != nil {
		return err
	}

	return nil
}

func WriteRootfsDir(tarWriter *tar.Writer) error {
	hdr := getGenericTarHeader()
	hdr.Name = "rootfs"
	hdr.Mode = 0755
	hdr.Size = int64(0)
	hdr.Typeflag = tar.TypeDir

	return tarWriter.WriteHeader(hdr)
}

func getGenericTarHeader() *tar.Header {
	// FIXME(iaguis) Use docker image time instead of the Unix Epoch?
	hdr := &tar.Header{
		Uid:        0,
		Gid:        0,
		ModTime:    time.Unix(0, 0),
		Uname:      "0",
		Gname:      "0",
		ChangeTime: time.Unix(0, 0),
	}

	return hdr
}
