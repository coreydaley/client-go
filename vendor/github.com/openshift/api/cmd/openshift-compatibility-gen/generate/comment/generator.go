package comment

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
	"github.com/dave/dst/dstutil"
	"k8s.io/gengo/types"
	"k8s.io/klog/v2"
)

const (
	baseTagName     = "openshift:compatibility-gen"
	levelTagName    = baseTagName + ":level"
	internalTagName = baseTagName + ":internal"
)

// GenerateCompatibilityComments add a compatibility level comment to instrumented types.
func GenerateCompatibilityComments(inputPkgs []string) error {
	for _, inputPkg := range inputPkgs {
		output, err := exec.Command("go", "list", "-f", "{{ .Dir }}", inputPkg).Output()
		if err != nil {
			klog.Errorf(string(output))
			return err
		}
		p := string(output)
		p = strings.TrimSpace(p)
		err = insertCompatibilityLevelComments(p)
		if err != nil {
			return err
		}
	}
	return nil
}

func insertCompatibilityLevelComments(path string) error {
	pkgs, err := decorator.ParseDir(token.NewFileSet(), path, onlyTypesFiles, parser.ParseComments)
	if err != nil {
		return err
	}
	for _, pkg := range pkgs {
		err = processPackage(pkg)
		if err != nil {
			return err
		}
	}
	return nil
}

// onlyTypesFiles returns true if there is a reasonable chance the file contain type definitions
func onlyTypesFiles(info os.FileInfo) bool {
	if strings.HasPrefix("zz_generated", info.Name()) {
		return false
	}
	switch info.Name() {
	case "doc.go", "register.go", "generated.pb.go":
		return false
	}
	return true
}

// processPackage processes all the files in a package
func processPackage(pkg *dst.Package) error {
	for fileName, file := range pkg.Files {
		fileChanged, err := processFile(file)
		if err != nil {
			return err
		}
		if !fileChanged {
			continue
		}
		removeIgnoreAutogeneratedBuildTag(file)
		var buf bytes.Buffer
		if err = decorator.Fprint(&buf, file); err != nil {
			return err
		}
		if err = ioutil.WriteFile(fileName, buf.Bytes(), 0777); err != nil {
			return err
		}
	}
	return nil
}

// removeIgnoreAutogeneratedBuildTag removes the `// +build !ignore_autogenerated` build tag that
// somehow gets added to the generated file.
func removeIgnoreAutogeneratedBuildTag(file *dst.File) {
	if len(file.Decls) > 0 {
		end := file.Decls[len(file.Decls)-1].Decorations().End
		if len(end.All()) > 0 {
			var filtered []string
			for _, comment := range end.All() {
				if comment != "// +build !ignore_autogenerated" {
					filtered = append(filtered, comment)
				}
			}
			file.Decls[len(file.Decls)-1].Decorations().End.Replace(filtered...)
		}
	}
}

// processFile adds compatibility level comments to a file
func processFile(f *dst.File) (bool, error) {
	g := compatibilityLevelCommentGenerator{}
	dstutil.Apply(f, nil, g.applyCompatibilityLevelComment())
	return g.changed, g.err
}

// compatibilityLevelCommentGenerator provides an ApplyFunc for dst.Apply() and knows if
// the ApplyFunc actually changed the source code.
type compatibilityLevelCommentGenerator struct {
	changed bool
	err     error
}

// applyCompatibilityLevelComment returns an ApplyFunc that inserts compatibility level comments.
func (g *compatibilityLevelCommentGenerator) applyCompatibilityLevelComment() dstutil.ApplyFunc {
	return func(c *dstutil.Cursor) bool {

		genDecl, ok := c.Node().(*dst.GenDecl)
		if !ok {
			return true
		}
		// we have a generic declaration

		if genDecl.Tok != token.TYPE {
			return true
		}
		// we have a type declaration

		typeSpec := genDecl.Specs[0].(*dst.TypeSpec)
		structType, ok := typeSpec.Type.(*dst.StructType)
		if !ok {
			return true
		}
		// we have a struct type declaration

		var isAPIType bool
		for _, field := range structType.Fields.List {
			if len(field.Names) != 0 {
				continue
			}
			selectorExpr, ok := field.Type.(*dst.SelectorExpr)
			if !ok {
				continue
			}
			if selectorExpr.Sel.Name != "TypeMeta" {
				continue
			}
			isAPIType = true
			break
		}
		if !isAPIType {
			return true
		}
		apiTypeName := typeSpec.Name.Name
		// we have an API Type
		klog.V(5).Infof("API type found: %v", apiTypeName)

		klog.V(5).Infof("Checking %v...", apiTypeName)
		klog.V(5).Infof("  Before  : %v", genDecl.Decorations().Before.String())
		klog.V(5).Infof("  After   : %v", genDecl.Decorations().After.String())
		klog.V(5).Infof("  Start   : %#v", genDecl.Decorations().Start.All())
		klog.V(5).Infof("  End     : %#v", genDecl.Decorations().End.All())

		internal := extractIsInternal(genDecl)
		klog.V(5).Infof("  Internal: %v", internal)

		level, ok := extractCompatibilityLevel(genDecl)
		if !internal && !ok {
			g.err = fmt.Errorf("%s: level or internal must be specified", apiTypeName)
			return false
		}
		if !ok {
			level = 4 // default level for internal types
		}
		klog.V(5).Infof("  Level   : %v", level)

		ga := versionIsGenerallyAvailable(c)
		beta := versionIsPrerelease(c)
		alpha := versionIsExperimental(c)

		klog.V(5).Infof("  GA/A/B  : %v/%v/%v", ga, beta, alpha)

		switch {
		case internal && level != 4:
			g.err = fmt.Errorf("%s: APIs that are not internal are only allowed to offer level 4 compatibility: long term support cannot be offered for the %s API", apiTypeName, apiTypeName)
			return false
		case internal:
		case !(ga || alpha || beta):
			g.err = fmt.Errorf("%s: APIs whose versions do not conform to kube apiVersion format cannot be exposed: the %s API must be tagged with +%s", apiTypeName, apiTypeName, internalTagName)
			return false
		case ga && level != 1:
			g.err = fmt.Errorf("%s: generally available APIs must be supported for a minimum of 12 months", apiTypeName)
			return false
		case beta && level == 1:
			g.err = fmt.Errorf("%s: pre-release (beta) APIs must offer level 2 compatibility: the %s API should be versioned as generally available if you with to offer level 1 compatibility", apiTypeName, apiTypeName)
			return false
		case beta && level == 4:
			g.err = fmt.Errorf("%s: pre-release (beta) APIs must offer level 2 compatibility: the %s API should be versioned as experimental (alpha) if you wish to offer level 4 compatibility", apiTypeName, apiTypeName)
			return false
		case alpha && level != 4:
			g.err = fmt.Errorf("%s: experimental (alpha) APIs are only allowed to offer level 4 compatibility: long term support cannot be offered for the %s API", apiTypeName, apiTypeName)
			return false
		}

		// we have a compatibility level tag

		// add/edit comments as needed
		changed := ensureCompatibilityLevelComment(genDecl, level)
		if changed {
			g.changed = true
		}

		// continue to process nodes
		return true
	}
}

func extractCompatibilityLevel(spec *dst.GenDecl) (int, bool) {
	tags := types.ExtractCommentTags("// +", spec.Decorations().Start.All())
	value, ok := tags[levelTagName]
	if !ok {
		return 0, false
	}
	level, err := strconv.Atoi(value[0])
	if err != nil {
		klog.Errorf("%s: unable to parse value of %s tag: %v", typeName(spec), levelTagName, err)
	}
	switch level {
	case 1, 2, 3, 4:
	default:
		klog.Errorf("%s: invalid value of %s tag: %v", typeName(spec), levelTagName, level)
	}
	return level, true
}

func extractIsInternal(spec *dst.GenDecl) bool {
	tags := types.ExtractCommentTags("// +", spec.Decorations().Start.All())
	value, ok := tags[internalTagName]
	if !ok {
		return false
	}
	if len(value[0]) == 0 {
		return true
	}
	internal, err := strconv.ParseBool(value[0])
	if err != nil {
		klog.Fatalf("%s: error parsing %s tag: %v", typeName(spec), err)
	}
	return internal
}

func typeName(spec *dst.GenDecl) string {
	return spec.Specs[0].(*dst.TypeSpec).Name.String()
}

func versionIsGenerallyAvailable(c *dstutil.Cursor) bool {
	return regexp.MustCompile(`^v\d*$`).MatchString(path.Base((c.Parent().(*dst.File)).Name.String()))
}

func versionIsPrerelease(c *dstutil.Cursor) bool {
	return regexp.MustCompile(`^v\d*beta\d*$`).MatchString(path.Base((c.Parent().(*dst.File)).Name.String()))
}

func versionIsExperimental(c *dstutil.Cursor) bool {
	return regexp.MustCompile(`^v\d*alpha\d*$`).MatchString(path.Base((c.Parent().(*dst.File)).Name.String()))
}

// ensureCompatibilityLevelComment either replaces a stale compatibility level comment, or adds a new one.
func ensureCompatibilityLevelComment(genDecl *dst.GenDecl, level int) bool {
	// copy of existing comments we can manipulate
	comments := append([]string{}, genDecl.Decorations().Start.All()...)

	newComment := fmt.Sprintf("// Compatibility level %d: %s", level, commentForLevel(level))

	// if there is already a compatibility comment, replace if needed
	for i, existingComment := range comments {
		switch {
		case existingComment == newComment:
			return false
		case strings.HasPrefix(existingComment, "// Compatibility level "):
			comments[i] = newComment
			genDecl.Decorations().Start.Replace(comments...)
			return true
		}
	}

	// no existing compatibility comment, find a nice place to add one
	insertIndex := len(comments)
l:
	for i := len(comments) - 1; i >= 0; i-- {
		switch {
		case strings.HasPrefix(comments[i], "// +"):
			insertIndex = i
		case comments[i] == "\n":
			// in order to show up in the godoc, comment must be adjacent to declaration
			break l
		}
	}

	// surround with empty ('//') comments if needed to ensure godoc paragraph breaks
	newComments := []string{
		newComment,
	}
	switch {
	case insertIndex == 0:
	case comments[insertIndex-1] == "\n":
	case comments[insertIndex-1] == "// ":
	default:
		newComments = append([]string{"// "}, newComments...)
	}
	switch {
	case insertIndex == len(comments):
	case comments[insertIndex] == "\n":
	case comments[insertIndex] == "// ":
	case strings.HasPrefix(comments[insertIndex], "// +"):
	default:
		newComments = append(newComments, "// ")
	}

	// insert comments
	comments = append(comments[:insertIndex], append(newComments, comments[insertIndex:]...)...)
	genDecl.Decorations().Start.Replace(comments...)

	return true
}

func commentForLevel(level int) string {
	switch level {
	case 1:
		return "Stable within a major release for a minimum of 12 months or 3 minor releases (whichever is longer)."
	case 2:
		return "Stable within a major release for a minimum of 9 months or 3 minor releases (whichever is longer)."
	case 3:
		return "Will attempt to be as compatible from version to version as possible, but version to version compatibility is not guaranteed."
	case 4:
		return "No compatibility is provided, the API can change at any point for any reason. These capabilities should not be used by applications needing long term support."
	default:
		panic(level)
	}
}
