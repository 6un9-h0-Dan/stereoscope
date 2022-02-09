package image

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/anchore/stereoscope/internal/bus"
	"github.com/anchore/stereoscope/internal/log"
	"github.com/anchore/stereoscope/pkg/event"
	"github.com/anchore/stereoscope/pkg/file"
	"github.com/anchore/stereoscope/pkg/filetree"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/wagoodman/go-partybus"
	"github.com/wagoodman/go-progress"
)

// Image represents a container image.
type Image struct {
	// image is the raw image metadata and content provider from the GCR lib
	image v1.Image
	// contentCacheDir is where all layer tar cache is stored.
	contentCacheDir string
	// Metadata contains select image attributes
	Metadata Metadata
	// Layers contains the rich layer objects in build order
	Layers []*Layer
	// FileCatalog contains all file metadata for all files in all layers
	FileCatalog FileCatalog

	overrideMetadata []AdditionalMetadata
}

type AdditionalMetadata func(*Image) error

func WithTags(tags ...string) AdditionalMetadata {
	return func(image *Image) error {
		var err error
		image.Metadata.Tags = make([]name.Tag, len(tags))
		for i, t := range tags {
			image.Metadata.Tags[i], err = name.NewTag(t)
			if err != nil {
				image.Metadata.Tags = nil
				break
			}
		}
		return err
	}
}

func WithManifest(manifest []byte) AdditionalMetadata {
	return func(image *Image) error {
		image.Metadata.RawManifest = manifest
		image.Metadata.ManifestDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(manifest))
		return nil
	}
}

func WithManifestDigest(digest string) AdditionalMetadata {
	return func(image *Image) error {
		image.Metadata.ManifestDigest = digest
		return nil
	}
}

func WithConfig(config []byte) AdditionalMetadata {
	return func(image *Image) error {
		image.Metadata.RawConfig = config
		image.Metadata.ID = fmt.Sprintf("sha256:%x", sha256.Sum256(config))
		return nil
	}
}

func WithRepoDigests(digests []string) AdditionalMetadata {
	return func(image *Image) error {
		if digests != nil {
			image.Metadata.RepoDigests = digests
		} else {
			image.Metadata.RepoDigests = []string{}
		}
		return nil
	}
}

// NewImage provides a new, unread image object.
func NewImage(image v1.Image, contentCacheDir string, additionalMetadata ...AdditionalMetadata) *Image {
	imgObj := &Image{
		image:            image,
		contentCacheDir:  contentCacheDir,
		FileCatalog:      NewFileCatalog(),
		overrideMetadata: additionalMetadata,
	}
	return imgObj
}

func (i *Image) IDs() []string {
	var ids = make([]string, len(i.Metadata.Tags))
	for idx, t := range i.Metadata.Tags {
		ids[idx] = t.String()
	}
	ids = append(ids, i.Metadata.ID)
	return ids
}

func (i *Image) trackReadProgress(metadata Metadata) *progress.Manual {
	prog := &progress.Manual{
		// x2 for read and squash of each layer
		Total: int64(len(metadata.Config.RootFS.DiffIDs) * 2),
	}

	bus.Publish(partybus.Event{
		Type:   event.ReadImage,
		Source: metadata,
		Value:  progress.Progressable(prog),
	})

	return prog
}

func (i *Image) applyOverrideMetadata() error {
	for _, optionFn := range i.overrideMetadata {
		if err := optionFn(i); err != nil {
			return fmt.Errorf("unable to override metadata option: %w", err)
		}
	}
	return nil
}

// Read parses information from the underlying image tar into this struct. This includes image metadata, layer
// metadata, layer file trees, and layer squash trees (which implies the image squash tree).
func (i *Image) Read() error {
	var layers = make([]*Layer, 0)
	var err error
	i.Metadata, err = readImageMetadata(i.image)
	if err != nil {
		return err
	}

	// override any metadata with what the user has provided manually
	if err = i.applyOverrideMetadata(); err != nil {
		return err
	}

	log.Debugf("image metadata: digest=%+v mediaType=%+v tags=%+v",
		i.Metadata.ID,
		i.Metadata.MediaType,
		i.Metadata.Tags)

	v1Layers, err := i.image.Layers()
	if err != nil {
		return err
	}

	// let consumers know of a monitorable event (image save + copy stages)
	readProg := i.trackReadProgress(i.Metadata)

	for idx, v1Layer := range v1Layers {
		layer := NewLayer(v1Layer)
		err := layer.Read(&i.FileCatalog, i.Metadata, idx, i.contentCacheDir)
		if err != nil {
			return err
		}
		i.Metadata.Size += layer.Metadata.Size
		layers = append(layers, layer)

		readProg.N++
	}

	i.Layers = layers

	// in order to resolve symlinks all squashed trees must be available
	return i.squash(readProg)
}

// squash generates a squash tree for each layer in the image. For instance, layer 2 squash =
// squash(layer 0, layer 1, layer 2), layer 3 squash = squash(layer 0, layer 1, layer 2, layer 3), and so on.
func (i *Image) squash(prog *progress.Manual) error {
	var lastSquashTree *filetree.FileTree

	for idx, layer := range i.Layers {
		if idx == 0 {
			lastSquashTree = layer.Tree
			layer.SquashedTree = layer.Tree
			continue
		}

		var unionTree = filetree.NewUnionFileTree()
		unionTree.PushTree(lastSquashTree)
		unionTree.PushTree(layer.Tree)

		squashedTree, err := unionTree.Squash()
		if err != nil {
			return fmt.Errorf("failed to squash tree %d: %w", idx, err)
		}

		layer.SquashedTree = squashedTree
		lastSquashTree = squashedTree

		prog.N++
	}

	prog.SetCompleted()

	return nil
}

// SquashedTree returns the pre-computed image squash file tree.
func (i *Image) SquashedTree() *filetree.FileTree {
	layerCount := len(i.Layers)

	if layerCount == 0 {
		return filetree.NewFileTree()
	}

	topLayer := i.Layers[layerCount-1]
	return topLayer.SquashedTree
}

// FileContentsFromSquash fetches file contents for a single path, relative to the image squash tree.
// If the path does not exist an error is returned.
func (i *Image) FileContentsFromSquash(path file.Path) (io.ReadCloser, error) {
	return fetchFileContentsByPath(i.SquashedTree(), &i.FileCatalog, path)
}

// FilesByMIMETypeFromSquash returns file references for files that match at least one of the given MIME types.
func (i *Image) FilesByMIMETypeFromSquash(mimeTypes ...string) ([]file.Reference, error) {
	var refs []file.Reference
	for _, ty := range mimeTypes {
		refsForType, err := fetchFilesByMIMEType(i.SquashedTree(), &i.FileCatalog, ty)
		if err != nil {
			return nil, err
		}
		refs = append(refs, refsForType...)
	}
	return refs, nil
}

// FileContentsByRef fetches file contents for a single file reference, irregardless of the source layer.
// If the path does not exist an error is returned.
func (i *Image) FileContentsByRef(ref file.Reference) (io.ReadCloser, error) {
	return i.FileCatalog.FileContents(ref)
}

// ResolveLinkByLayerSquash resolves a symlink or hardlink for the given file reference relative to the result from
// the layer squash of the given layer index argument.
// If the given file reference is not a link type, or is a unresolvable (dead) link, then the given file reference is returned.
func (i *Image) ResolveLinkByLayerSquash(ref file.Reference, layer int, options ...filetree.LinkResolutionOption) (*file.Reference, error) {
	allOptions := append([]filetree.LinkResolutionOption{filetree.FollowBasenameLinks}, options...)
	_, resolvedRef, err := i.Layers[layer].SquashedTree.File(ref.RealPath, allOptions...)
	return resolvedRef, err
}

// ResolveLinkByImageSquash resolves a symlink or hardlink for the given file reference relative to the result from the image squash.
// If the given file reference is not a link type, or is a unresolvable (dead) link, then the given file reference is returned.
func (i *Image) ResolveLinkByImageSquash(ref file.Reference, options ...filetree.LinkResolutionOption) (*file.Reference, error) {
	allOptions := append([]filetree.LinkResolutionOption{filetree.FollowBasenameLinks}, options...)
	_, resolvedRef, err := i.Layers[len(i.Layers)-1].SquashedTree.File(ref.RealPath, allOptions...)
	return resolvedRef, err
}

// Cleanup removes all temporary files created from parsing the image. Future calls to image will not function correctly after this call.
func (i *Image) Cleanup() error {
	if i.contentCacheDir != "" {
		if err := os.RemoveAll(i.contentCacheDir); err != nil {
			return err
		}
	}
	return nil
}
