package imagestore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/kelseyhightower/envconfig"
	"github.com/openshift/assisted-image-service/pkg/isoeditor"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

var DefaultVersions = map[string]map[string]string{
	"4.6": {
		"iso_url":    "https://mirror.openshift.com/pub/openshift-v4/dependencies/rhcos/4.6/4.6.8/rhcos-4.6.8-x86_64-live.x86_64.iso",
		"rootfs_url": "https://mirror.openshift.com/pub/openshift-v4/dependencies/rhcos/4.6/4.6.8/rhcos-live-rootfs.x86_64.img",
	},
	"4.7": {
		"iso_url":    "https://mirror.openshift.com/pub/openshift-v4/dependencies/rhcos/4.7/4.7.13/rhcos-4.7.13-x86_64-live.x86_64.iso",
		"rootfs_url": "https://mirror.openshift.com/pub/openshift-v4/dependencies/rhcos/4.7/4.7.13/rhcos-live-rootfs.x86_64.img",
	},
	"4.8": {
		"iso_url":    "https://mirror.openshift.com/pub/openshift-v4/dependencies/rhcos/pre-release/4.8.0-rc.3/rhcos-4.8.0-rc.3-x86_64-live.x86_64.iso",
		"rootfs_url": "https://mirror.openshift.com/pub/openshift-v4/dependencies/rhcos/pre-release/4.8.0-rc.3/rhcos-live-rootfs.x86_64.img",
	},
}

//go:generate mockgen -package=imagestore -destination=mock_imagestore.go . ImageStore
type ImageStore interface {
	Populate(ctx context.Context) error
	BaseFile(version, imageType string) (string, error)
	HaveVersion(version string) bool
}

type Config struct {
	Versions string `envconfig:"RHCOS_VERSIONS"`
}

type rhcosStore struct {
	cfg       *Config
	versions  map[string]map[string]string
	isoEditor isoeditor.Editor
	dataDir   string
}

const (
	ImageTypeFull    = "full"
	ImageTypeMinimal = "minimal"
)

func NewImageStore(ed isoeditor.Editor, dataDir string) (ImageStore, error) {
	cfg := &Config{}
	err := envconfig.Process("image-store", cfg)
	if err != nil {
		return nil, err
	}
	is := rhcosStore{
		cfg:       cfg,
		isoEditor: ed,
		dataDir:   dataDir,
	}
	if cfg.Versions == "" {
		is.versions = DefaultVersions
	} else {
		err = json.Unmarshal([]byte(cfg.Versions), &is.versions)
		if err != nil {
			return nil, err
		}
	}
	return &is, nil
}

func downloadURLToFile(url string, path string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("Request to %s returned error code %d", url, resp.StatusCode)
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	count, err := io.Copy(f, resp.Body)
	if err != nil {
		return err
	} else if count != resp.ContentLength {
		return fmt.Errorf("Wrote %d bytes, but expected to write %d", count, resp.ContentLength)
	}

	return nil
}

func (s *rhcosStore) Populate(ctx context.Context) error {
	errs, _ := errgroup.WithContext(ctx)

	for version := range s.versions {
		version := version
		errs.Go(func() error {
			fullPath, err := s.pathForVersion(version)
			if err != nil {
				return err
			}

			if _, err = os.Stat(fullPath); os.IsNotExist(err) {
				url := s.versions[version]["iso_url"]
				log.Infof("Downloading iso from %s to %s", url, fullPath)
				err = downloadURLToFile(url, fullPath)
				if err != nil {
					return fmt.Errorf("failed to download %s: %v", url, err)
				}
				log.Infof("Finished downloading for version %s", version)
			}

			minimalPath, err := s.minimalPathForVersion(version)
			if err != nil {
				return err
			}

			if _, err = os.Stat(minimalPath); os.IsNotExist(err) {
				log.Infof("Creating minimal iso for version %s", version)

				rootfsURL, err := s.rootfsURLForVersion(version)
				if err != nil {
					return err
				}

				err = s.isoEditor.CreateMinimalISOTemplate(fullPath, rootfsURL, minimalPath)
				if err != nil {
					return fmt.Errorf("failed to create minimal iso template for version %s: %v", version, err)
				}
				log.Infof("Finished creating minimal iso for version %s", version)
			}

			return nil
		})
	}

	return errs.Wait()
}

func (s *rhcosStore) rootfsURLForVersion(version string) (string, error) {
	v, ok := s.versions[version]
	if !ok {
		return "", fmt.Errorf("missing version entry for %s", version)
	}
	url, ok := v["rootfs_url"]
	if !ok {
		return "", fmt.Errorf("version %s missing key 'rootfs_url'", version)
	}
	return url, nil
}

func (s *rhcosStore) pathForVersion(version string) (string, error) {
	v, ok := s.versions[version]
	if !ok {
		return "", fmt.Errorf("missing version entry for %s", version)
	}
	url, ok := v["iso_url"]
	if !ok {
		return "", fmt.Errorf("version %s missing key 'iso_url'", version)
	}
	return filepath.Join(s.dataDir, filepath.Base(url)), nil
}

func (s *rhcosStore) minimalPathForVersion(version string) (string, error) {
	v, ok := s.versions[version]
	if !ok {
		return "", fmt.Errorf("missing version entry for %s", version)
	}
	url, ok := v["iso_url"]
	if !ok {
		return "", fmt.Errorf("version %s missing key 'iso_url'", version)
	}
	return filepath.Join(s.dataDir, "minimal-"+filepath.Base(url)), nil
}

func (s *rhcosStore) BaseFile(version, imageType string) (string, error) {
	var (
		path string
		err  error
	)

	switch imageType {
	case ImageTypeFull:
		path, err = s.pathForVersion(version)
	case ImageTypeMinimal:
		path, err = s.minimalPathForVersion(version)
	default:
		err = fmt.Errorf("unsupported image type '%s'", imageType)
	}
	if err != nil {
		return "", err
	}

	return path, nil
}

func (s *rhcosStore) HaveVersion(version string) bool {
	_, ok := s.versions[version]
	return ok
}
