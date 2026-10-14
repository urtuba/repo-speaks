package web

import (
	"bytes"
	"html/template"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// An answer is the model's text, and the model read files from
// repositories that anyone could have written. Such a file can tell the
// model to put a link or an image in the answer, for example one that
// sends the chat to another site. So answers render with raw HTML off,
// links show as plain text with their address, and images are never
// loaded. Only the source links the app builds are clickable.

// answerMarkdown renders answers. Tables and strikethrough are on;
// automatic links are off, so a bare address stays text.
var answerMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.Strikethrough),
	// The default renderer has priority 1000. A lower number wins, so
	// these functions replace its links and images.
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(plainLinks{}, 100))),
)

// renderAnswer turns an answer's Markdown into HTML. Raw HTML in it is
// left out: goldmark drops it unless told otherwise.
func renderAnswer(md string) template.HTML {
	var buf bytes.Buffer
	if err := answerMarkdown.Convert([]byte(md), &buf); err != nil {
		// Convert only fails when writing fails, which a buffer does not.
		return template.HTML(template.HTMLEscapeString(md))
	}
	return template.HTML(buf.String())
}

// plainLinks renders links and images as text.
type plainLinks struct{}

func (plainLinks) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindLink, renderLink)
	reg.Register(ast.KindAutoLink, renderAutoLink)
	reg.Register(ast.KindImage, renderImage)
}

// renderLink writes a link's text, then its address in parentheses.
func renderLink(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		w.WriteString(" (")
		w.Write(util.EscapeHTML(n.(*ast.Link).Destination))
		w.WriteString(")")
	}
	return ast.WalkContinue, nil
}

// renderAutoLink writes an address in angle brackets as text.
func renderAutoLink(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		w.Write(util.EscapeHTML(n.(*ast.AutoLink).Label(source)))
	}
	return ast.WalkSkipChildren, nil
}

// renderImage writes an image's description, then its address, and
// never loads it.
func renderImage(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		w.WriteString(" (image not shown: ")
		w.Write(util.EscapeHTML(n.(*ast.Image).Destination))
		w.WriteString(")")
	}
	return ast.WalkContinue, nil
}
