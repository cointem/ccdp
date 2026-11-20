package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"ccdp/internal/fsops"
	"ccdp/internal/protocol"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
)

type inspectorFile struct {
	list                        ui.ListState
	workspace, path             string
	entries                     []os.DirEntry
	directory, loading, clipped bool
	text, problem               string
}

func projectRelativePath(workspace, path string) (string, error) {
	if workspace == "" {
		return "", errors.New("请先选择项目")
	}
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(workspace, path)
		if err != nil {
			return "", err
		}
		path = rel
	}
	path = filepath.Clean(path)
	if !filepath.IsLocal(path) {
		return "", errors.New("文件位于当前项目之外")
	}
	return path, nil
}

func loadInspectorFile(workspace, path string) (inspectorFile, error) {
	p := inspectorFile{workspace: workspace, path: path}
	rel, err := projectRelativePath(workspace, path)
	if err != nil {
		return p, err
	}
	p.path = rel
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return p, err
	}
	defer root.Close()
	fi, err := root.Lstat(rel)
	if err != nil {
		return p, err
	}
	if fi.IsDir() {
		p.directory = true
		f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return p, err
		}
		defer f.Close()
		p.entries, err = f.ReadDir(-1)
		if err != nil {
			return p, err
		}
		slices.SortFunc(p.entries, func(a, b os.DirEntry) int {
			if a.IsDir() != b.IsDir() {
				if a.IsDir() {
					return -1
				}
				return 1
			}
			return strings.Compare(a.Name(), b.Name())
		})
		return p, nil
	}
	if !fi.Mode().IsRegular() {
		return p, errors.New("此文件类型不支持文本预览")
	}
	f, err := fsops.OpenRegularAt(root, rel)
	if err != nil {
		return p, err
	}
	defer f.Close()
	const limit = 96 << 10
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return p, err
	}
	if len(data) > limit {
		p.clipped = true
		n := limit
		for n > 0 && !utf8.RuneStart(data[n]) {
			n--
		}
		data = data[:n]
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return p, errors.New("此文件不支持文本预览")
	}
	p.text = string(data)
	return p, nil
}

func (a *app) chatMarkdown(c *ui.Context, source *conversation, text string, streaming bool) {
	body := ui.Column(c).MinWidth(0)
	opts := *ui.Local(body, "markdown-options", func() *markdown.Options { return &markdown.Options{} })
	opts.OnLink = func(dest string) {
		if strings.HasPrefix(dest, "#") {
			opts.Anchor = dest
			opts.AnchorVersion++
			return
		}
		if ui.SafeToOpen(dest) {
			c.OpenURL(dest)
			return
		}
		if path, line, ok := localFileLink(dest); ok {
			a.inspectFileAt(source, path, line)
		}
	}
	body.Children(func() {
		if streaming {
			markdown.Stream(c, text, opts)
		} else {
			markdown.View(c, text, opts)
		}
	})
}

// Accept workspace-relative and absolute file links, with :line or #Lline.
func localFileLink(dest string) (string, int, bool) {
	if dest == "" || strings.HasPrefix(dest, "//") {
		return "", 0, false
	}
	raw, frag, _ := strings.Cut(dest, "#")
	line, _ := strconv.Atoi(strings.TrimPrefix(frag, "L"))
	if path, suffix, ok := cutLineSuffix(raw); ok {
		raw, line = path, suffix
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "" && u.Scheme != "file") || (u.Host != "" && !(u.Scheme == "file" && u.Host == "localhost")) || u.Path == "" {
		return "", 0, false
	}
	return u.Path, max(0, line), true
}

func cutLineSuffix(path string) (string, int, bool) {
	i := strings.LastIndexByte(path, ':')
	if i < 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(path[i+1:])
	return path[:i], n, err == nil && n > 0
}

func (a *app) inspectFile(conv *conversation, path string) {
	a.inspectFileAt(conv, path, 0)
}

func (a *app) inspectFileAt(conv *conversation, path string, line int) {
	rel, err := projectRelativePath(conv.view.Settings.Workspace, path)
	if err != nil {
		conv.notice = err.Error()
		return
	}
	title := filepath.Base(rel)
	if rel == "." {
		title = filepath.Base(conv.view.Settings.Workspace)
	}
	page := a.inspectorFor(conv).selectPage("file:"+rel, title, "book", conv)
	page.line, page.revealLine = line, line > 0
	page.activity = nil
	page.glyph = "book"
	if page.file == nil {
		a.loadFilePage(page, rel)
	}
}

func (a *app) loadFilePage(page *inspectorPage, path string) {
	workspace := page.source.view.Settings.Workspace
	request := &inspectorFile{workspace: workspace, path: path, loading: true}
	page.file = request
	go func() {
		file, err := loadInspectorFile(workspace, path)
		if err != nil {
			file.problem = err.Error()
		}
		a.dispatch(func() {
			if page.file == request {
				page.file = &file
			}
		})
	}()
}

func (a *app) filePanel(c *ui.Context, page *inspectorPage) {
	f, conv := page.file, page.source
	t := c.Theme()
	ui.Row(c).AlignItems(ui.Center).Gap(5).Padding(4, 8).Children(func() {
		if f.directory && f.path != "." {
			iconButton(c, "back", "上级目录").OnClick(func() { a.inspectFile(conv, filepath.Dir(f.path)) })
		}
		title := f.path
		if title == "." {
			title = filepath.Base(f.workspace)
		}
		if f.workspace == "" {
			title = "项目文件"
		}
		ui.Text(c, title).SingleLine().Grow(1).MinWidth(0).FontSize(13).FontWeight(600)
		iconButton(c, "refresh", "刷新文件").OnClick(func() { a.loadFilePage(page, f.path) })
	})
	if f.directory && f.problem == "" {
		// The list owns scrolling and only builds entries inside the viewport.
		if len(f.entries) == 0 {
			ui.Text(c, "空目录").Padding(12).TextColor(t.TextMuted)
			return
		}
		ui.List(c.Key(f), &f.list, len(f.entries), func(i int) {
			entry := f.entries[i]
			glyph := "book"
			if entry.IsDir() {
				glyph = "folder"
			}
			quietButton(c, glyph, entry.Name()).OnClick(func() { a.inspectFile(conv, filepath.Join(f.path, entry.Name())) })
		}).Grow(1).MinHeight(0).Padding(4, 8)
		return
	}
	ui.Scroll(c.Key(f)).Grow(1).MinHeight(0).Padding(12).Gap(8).Children(func() {
		switch {
		case f.loading:
			ui.Spinner(c).Size(16, 16).Label("读取文件")
		case f.problem != "":
			ui.Text(c, f.problem).Selectable().TextColor(t.TextMuted)
		case f.text == "":
			ui.Text(c, "空文件").TextColor(t.TextMuted)
		default:
			text := func(value string) ui.Element { return ui.Text(c, value).Font("monospace").FontSize(12).Selectable() }
			if page.line <= 0 {
				text(f.text)
				break
			}
			lines := strings.Split(f.text, "\n")
			if page.line > len(lines) {
				text(f.text)
				break
			}
			if page.line > 1 {
				text(strings.Join(lines[:page.line-1], "\n"))
			}
			row := text(lines[page.line-1]).Background(t.SurfaceHover)
			if page.revealLine {
				row.ScrollIntoView()
				page.revealLine = false
			}
			if page.line < len(lines) {
				text(strings.Join(lines[page.line:], "\n"))
			}
		}
		if f.clipped {
			ui.Text(c, "已显示前 96 KB").FontSize(11).TextColor(t.TextMuted)
		}
	})
}

type fileEdit struct {
	Path    string `json:"file_path"`
	Content string `json:"content"`
	Edits   []struct {
		Old string `json:"old_text"`
		New string `json:"new_text"`
	} `json:"edits"`
}

// Open the exact tool invocation the user clicked. The recorded source or edit
// remains readable when the working tree subsequently changes.
func (a *app) openFileActivity(conv *conversation, item protocol.TranscriptItem, path string) {
	key := filepath.Clean(path)
	if rel, err := projectRelativePath(conv.view.Settings.Workspace, path); err == nil {
		key = rel
	}
	page := a.inspectorFor(conv).selectPage("file:"+key, filepath.Base(path), "book", conv)
	page.file = nil
	page.activity = &item
	if !strings.EqualFold(item.Tool, "Read") {
		page.glyph = "edit"
	}
}

func (a *app) activityPanel(c *ui.Context, page *inspectorPage) {
	item, conv := page.activity, page.source
	var f fileEdit
	_ = json.Unmarshal(item.Args, &f)
	path, err := projectRelativePath(conv.view.Settings.Workspace, f.Path)
	if err != nil {
		path = f.Path
	}
	t := c.Theme()
	ui.Row(c).AlignItems(ui.Center).Gap(5).Padding(5, 10).Children(func() {
		ui.Text(c, path).SingleLine().Grow(1).MinWidth(0).FontSize(13).Tooltip(f.Path)
		if err == nil {
			iconButton(c, "folder", "查看当前文件").OnClick(func() { a.inspectFile(conv, path) })
		}
	})
	ui.Scroll(c).Grow(1).MinHeight(0).Padding(12).Gap(6).Children(func() {
		switch strings.ToLower(item.Tool) {
		case "edit":
			for _, e := range f.Edits {
				for _, part := range []struct {
					text, prefix string
					color        ui.Color
				}{{e.Old, "− ", ui.Hex("#b55e59")}, {e.New, "+ ", ui.Hex("#47886b")}} {
					if part.text != "" {
						ui.Text(c, part.prefix+strings.ReplaceAll(strings.TrimSuffix(part.text, "\n"), "\n", "\n"+part.prefix)).Font("monospace").FontSize(12).TextColor(part.color).Selectable()
					}
				}
			}
			if len(f.Edits) > 0 {
				break
			}
			fallthrough
		default:
			ui.Text(c, item.Text).Font("monospace").FontSize(12).Selectable()
		case "write":
			ui.Text(c, f.Content).Font("monospace").FontSize(12).Selectable()
		}
		if item.Truncated {
			ui.Text(c, "记录已截断").FontSize(11).TextColor(t.TextMuted)
		}
	})
}
