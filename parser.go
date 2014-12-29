package wellington

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wellington/wellington/context"
	// TODO: Remove dot imports
	"github.com/wellington/wellington/lexer"
	"github.com/wellington/wellington/token"
)

var weAreNeverGettingBackTogether = []byte(`@mixin sprite-dimensions($map, $name) {
  $file: sprite-file($map, $name);
  height: image-height($file);
  width: image-width($file);
}
`)

func init() {
	log.SetFlags(log.Lshortfile | log.Ldate | log.Ltime)
}

// Replace holds token values for replacing source input with parsed input.
// DEPRECATED
type Replace struct {
	Start, End int
	Value      []byte
}

// Parser represents a parser engine that returns parsed and imported code
// from the input useful for doing text manipulation before passing to libsass.
type Parser struct {
	Idx, shift           int
	Chop                 []Replace
	Pwd, Input, MainFile string
	SassDir, BuildDir,

	ProjDir string
	ImageDir   string
	Includes   []string
	Items      []lexer.Item
	Output     []byte
	Line       map[int]string
	LineKeys   []int
	PartialMap *SafePartialMap
}

// NewParser returns a pointer to a Parser object.
func NewParser() *Parser {
	return &Parser{PartialMap: NewPartialMap()}
}

// Start reads the tokens from the lexer and performs
// conversions and/or substitutions for sprite*() calls.
//
// Start creates a map of all variables and sprites
// (created via sprite-map calls).
// TODO: Remove pkgdir, it can be put on Parser
func (p *Parser) Start(in io.Reader, pkgdir string) ([]byte, error) {
	p.Line = make(map[int]string)

	// Setup paths
	if p.MainFile == "" {
		p.MainFile = "string"
	}
	if p.BuildDir == "" {
		p.BuildDir = pkgdir
	}
	if p.SassDir == "" {
		p.SassDir = pkgdir
	}
	buf := bytes.NewBuffer(make([]byte, 0, bytes.MinRead))
	if in == nil {
		return []byte{}, fmt.Errorf("input is empty")
	}
	_, err := buf.ReadFrom(in)
	if err != nil {
		return []byte{}, err
	}

	// This pass resolves all the imports, but positions will
	// be off due to @import calls
	items, input, err := p.GetItems(pkgdir, p.MainFile, string(buf.Bytes()))
	if err != nil {
		return []byte(""), err
	}
	for i := range p.Line {
		p.LineKeys = append(p.LineKeys, i)
	}
	sort.Ints(p.LineKeys)
	// Try removing this and see if it works
	// This call will have valid token positions
	// items, input, err = p.GetItems(pkgdir, p.MainFile, input)

	p.Input = input
	p.Items = items
	if err != nil {
		panic(err)
	}
	// DEBUG
	// for _, item := range p.Items {
	// 	fmt.Printf("%s %s\n", item.Type, item)
	// }
	// Process sprite calls and gen

	// Parsing is no longer necessary
	// p.Parse(p.Items)
	p.Output = []byte(p.Input)
	// Perform substitutions
	// p.Replace()
	// rel := []byte(fmt.Sprintf(`$rel: "%s";%s`,
	//   p.Rel(), "\n"))

	// Code that we will never support, ever

	return append(weAreNeverGettingBackTogether, p.Output...), nil
}

// LookupFile translates line positions into line number
// and file it belongs to
func (p *Parser) LookupFile(position int) string {
	// Shift to 0 index
	pos := position - 1
	// Adjust for shift from preamble
	shift := bytes.Count(weAreNeverGettingBackTogether, []byte{'\n'})
	pos = pos - shift
	if pos < 0 {
		return "mixin"
	}
	for i, n := range p.LineKeys {
		if n > pos {
			if i == 0 {
				// Return 1 index line numbers
				return fmt.Sprintf("%s:%d", p.Line[i], pos+1)
			}
			hit := p.LineKeys[i-1]
			filename := p.Line[hit]
			// Catch for mainimport errors
			if filename == "string" {
				filename = p.MainFile
			}
			return fmt.Sprintf("%s:%d", filename, pos-hit+1)
		}
	}
	// Either this is invalid or outside of all imports, assume it's valid
	return fmt.Sprintf("%s:%d", p.MainFile, pos-p.LineKeys[len(p.LineKeys)-1]+1)
}

// GetItems recursively resolves all imports.  It lexes the input
// adding the tokens to the Parser object.
// TODO: Convert this to byte slice in/out
func (p *Parser) GetItems(pwd, filename, input string) ([]lexer.Item, string, error) {

	var (
		status    []lexer.Item
		importing bool
		output    []byte
		pos       int
		last      *lexer.Item
		lastname  string
		lineCount int
	)

	lex := lexer.New(func(lex *lexer.Lexer) lexer.StateFn {
		return lex.Action()
	}, input)

	for {
		item := lex.Next()
		err := item.Error()
		//fmt.Println(item.Type, item.Value)
		if err != nil {
			return nil, string(output),
				fmt.Errorf("Error: %v (pos %d)", err, item.Pos)
		}
		switch item.Type {
		case token.ItemEOF:
			if filename == p.MainFile {
				p.Line[lineCount+bytes.Count([]byte(input[pos:]), []byte("\n"))] = filename
			}
			output = append(output, input[pos:]...)
			return status, string(output), nil
		case token.IMPORT:
			output = append(output, input[pos:item.Pos]...)
			last = item
			importing = true
		case token.INCLUDE, token.CMT:
			output = append(output, input[pos:item.Pos]...)
			pos = item.Pos
			status = append(status, *item)
		default:
			if importing {
				lastname = filename
				// Found import, mark parent's current position
				p.Line[lineCount] = filename
				filename = fmt.Sprintf("%s", *item)
				for _, nl := range output {
					if nl == '\n' {
						lineCount++
					}
				}
				p.Line[lineCount] = filename
				pwd, contents, err := p.ImportPath(pwd, filename)

				if err != nil {
					return nil, "", err
				}
				//Eat the semicolon
				item := lex.Next()
				if item.Type != token.SEMIC {
					log.Printf("@import in %s:%d must be followed by ;\n", filename, lineCount)
					log.Printf("        ~~~> @import %s", filename)
				}
				// Set position to token after
				// FIXME: Hack to delete newline, hopefully this doesn't break stuff
				// then readd it to the linecount
				pos = item.Pos + len(item.Value)
				moreTokens, moreOutput, err := p.GetItems(
					pwd,
					filename,
					contents)
				// If importing was successful, each token must be moved
				// forward by the position of the @import call that made
				// it available.
				for i := range moreTokens {
					moreTokens[i].Pos += last.Pos
				}

				if err != nil {
					return nil, "", err
				}
				for _, nl := range moreOutput {
					if nl == '\n' {
						lineCount++
					}
				}
				filename = lastname

				output = append(output, moreOutput...)
				status = append(status, moreTokens...)
				importing = false
			} else {
				output = append(output, input[pos:item.Pos]...)
				pos = item.Pos
				status = append(status, *item)
			}
		}
	}

}

// LoadAndBuild kicks off parser and compiling
// TODO: make this function testable
func LoadAndBuild(sassFile string, gba *BuildArgs, partialMap *SafePartialMap) error {

	// Remove partials
	if strings.HasPrefix(filepath.Base(sassFile), "_") {
		return nil
	}

	if gba == nil {
		return fmt.Errorf("build args are nil")
	}

	// If no imagedir specified, assume relative to the input file
	if gba.Dir == "" {
		gba.Dir = filepath.Dir(sassFile)
	}
	var (
		out  io.WriteCloser
		fout string
	)
	if gba.BuildDir != "" {
		// Build output file based off build directory and input filename
		rel, _ := filepath.Rel(gba.Includes, filepath.Dir(sassFile))
		filename := strings.Replace(filepath.Base(sassFile), ".scss", ".css", 1)
		fout = filepath.Join(gba.BuildDir, rel, filename)
	} else {
		out = os.Stdout
	}
	ctx := context.Context{
		Sprites:     gba.Sprites,
		Imgs:        gba.Imgs,
		OutputStyle: gba.Style,
		ImageDir:    gba.Dir,
		FontDir:     gba.Font,
		// Assumption that output is a file
		BuildDir:     filepath.Dir(fout),
		GenImgDir:    gba.Gen,
		MainFile:     sassFile,
		Comments:     gba.Comments,
		IncludePaths: []string{filepath.Dir(sassFile)},
	}
	if gba.Includes != "" {
		ctx.IncludePaths = append(ctx.IncludePaths,
			strings.Split(gba.Includes, ",")...)
	}
	fRead, err := os.Open(sassFile)
	defer fRead.Close()
	if err != nil {
		return err
	}
	if fout != "" {
		dir := filepath.Dir(fout)
		err := os.MkdirAll(dir, 0755)
		if err != nil {
			return fmt.Errorf("Failed to create directory: %s", dir)
		}

		out, err = os.Create(fout)
		defer out.Close()
		if err != nil {
			return fmt.Errorf("Failed to create file: %s", sassFile)
		}
		// log.Println("Created:", fout)
	}

	var pout bytes.Buffer
	par, err := StartParser(&ctx, fRead, &pout, partialMap)
	if err != nil {
		return err
	}
	err = ctx.Compile(&pout, out)

	if err != nil {
		log.Println(ctx.MainFile)
		n := ctx.ErrorLine()
		fs := par.LookupFile(n)
		log.Printf("Error encountered in: %s\n", fs)
		return err
	}
	fmt.Printf("Rebuilt: %s\n", sassFile)
	return nil
}

// StartParser accepts build arguments
// TODO: Remove pkgdir, can be referenced from context
// TODO: Should this be called StartParser or NewParser?
// TODO: Should this function create the partialMap or is this
// the right way to inject one?
func StartParser(ctx *context.Context, in io.Reader, out io.Writer, partialMap *SafePartialMap) (*Parser, error) {
	// Run the sprite_sass parser prior to passing to libsass
	parser := &Parser{
		ImageDir:   ctx.ImageDir,
		Includes:   ctx.IncludePaths,
		BuildDir:   ctx.BuildDir,
		MainFile:   ctx.MainFile,
		PartialMap: partialMap,
	}
	// Save reference to parser in context
	bs, err := parser.Start(in, filepath.Dir(ctx.MainFile))
	if err != nil {
		return parser, err
	}
	out.Write(bs)
	return parser, err
}
