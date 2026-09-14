package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"ccdp/internal/protocol"
)

// ClipboardReader is separate from Clipboard so existing copy-only adapters
// remain compatible. Reads happen only on an explicit paste gesture.
type ClipboardReader interface {
	ReadClipboard(context.Context) (ClipboardContent, error)
}

type ClipboardContent struct {
	Text   string
	Images []protocol.InputImage
}

var clipboardReadTimeout = 5 * time.Second

// No shell interpolation, shared temporary filenames, clipboard polling, or
// clipboard writes. stdout is capped even if a helper produces unbounded data.
type clipboardCommand func(context.Context, int, string, ...string) ([]byte, error)

type boundedClipboardOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedClipboardOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		return 0, errors.New("clipboard output exceeds size limit")
	}
	return b.buffer.Write(p)
}

func runClipboardCommand(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out := &boundedClipboardOutput{limit: limit}
	errOut := &boundedClipboardOutput{limit: 4096}
	cmd.Stdout = out
	cmd.Stderr = errOut
	cmd.WaitDelay = 200 * time.Millisecond
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		detail := strings.TrimSpace(sanitizeANSI(errOut.buffer.String()))
		if detail != "" {
			return nil, fmt.Errorf("%s clipboard read failed: %w (%s)", name, err, detail)
		}
		return nil, fmt.Errorf("%s clipboard read failed: %w", name, err)
	}
	return out.buffer.Bytes(), nil
}

// JXA uses the system pasteboard without requiring cgo or a third-party
// binary. Screenshots normally supply PNG; TIFF-only applications are
// converted after checking encoded size and dimensions. Finder image copies
// can also be decoded through an explicitly copied file URL.
const macClipboardImageScript = `ObjC.import('AppKit');
function run() {
  var pb = $.NSPasteboard.generalPasteboard;
  if (!ObjC.unwrap(pb)) throw Error('desktop clipboard unavailable');
  var data = pb.dataForType('public.png');
  if (ObjC.unwrap(data)) {
    if (data.length > 20971520) throw Error('image exceeds 20 MiB');
    return ObjC.unwrap(data.base64EncodedStringWithOptions(0));
  }
  data = pb.dataForType('public.tiff');
  if (!ObjC.unwrap(data)) {
    var url = pb.stringForType('public.file-url');
    if (!ObjC.unwrap(url)) return '';
    var file = $.NSURL.URLWithString(url);
    if (!ObjC.unwrap(file) || !file.isFileURL) return '';
    var ext = ObjC.unwrap(file.pathExtension).toLowerCase();
    if (['png','jpg','jpeg','gif','webp','tif','tiff'].indexOf(ext) < 0) return '';
    var attrs = $.NSFileManager.defaultManager.attributesOfItemAtPathError(file.path, null);
    if (!ObjC.unwrap(attrs) || Number(ObjC.unwrap(attrs.objectForKey($.NSFileSize))) > 20971520) throw Error('image exceeds 20 MiB');
    data = $.NSData.dataWithContentsOfURL(file);
  }
  if (!ObjC.unwrap(data)) throw Error('cannot read clipboard image');
  if (data.length > 20971520) throw Error('image exceeds 20 MiB');
  var rep = $.NSBitmapImageRep.imageRepWithData(data);
  if (!ObjC.unwrap(rep)) throw Error('cannot decode clipboard image');
  if (rep.pixelsWide > 16384 || rep.pixelsHigh > 16384 || rep.pixelsWide * rep.pixelsHigh > 40000000) throw Error('image dimensions exceed limit');
  var png = rep.representationUsingTypeProperties($.NSPNGFileType, $.NSDictionary.dictionary);
  if (!ObjC.unwrap(png) || png.length > 20971520) throw Error('encoded image exceeds 20 MiB');
  return ObjC.unwrap(png.base64EncodedStringWithOptions(0));
}`

const windowsClipboardImageScript = `$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$img = [System.Windows.Forms.Clipboard]::GetImage()
if ($null -eq $img) { exit 0 }
try {
  if ($img.Width -gt 16384 -or $img.Height -gt 16384 -or ([long]$img.Width * $img.Height) -gt 40000000) { throw 'image dimensions exceed limit' }
  $stream = New-Object System.IO.MemoryStream
  try {
    $img.Save($stream, [System.Drawing.Imaging.ImageFormat]::Png)
    if ($stream.Length -gt 20971520) { throw 'image exceeds 20 MiB' }
    [Console]::Write([Convert]::ToBase64String($stream.ToArray()))
  } finally { $stream.Dispose() }
} catch { exit 1 } finally { $img.Dispose() }`

func (hostClipboard) ReadClipboard(ctx context.Context) (ClipboardContent, error) {
	wsl := os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != ""
	return readHostClipboard(ctx, runtime.GOOS, wsl, os.Getenv("WAYLAND_DISPLAY") != "", runClipboardCommand)
}

func readHostClipboard(ctx context.Context, platform string, wsl, wayland bool, run clipboardCommand) (ClipboardContent, error) {
	const encodedLimit = (protocol.MaxInputImageBytes+2)/3*4 + 1024
	if platform == "darwin" || platform == "windows" || wsl {
		name, args := "/usr/bin/osascript", []string{"-l", "JavaScript", "-e", macClipboardImageScript}
		if platform == "windows" || wsl {
			name, args = "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-STA", "-Command", windowsClipboardImageScript}
		}
		encoded, err := run(ctx, encodedLimit, name, args...)
		if err != nil {
			return ClipboardContent{}, err
		}
		if len(bytes.TrimSpace(encoded)) > 0 {
			data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
			if err != nil {
				return ClipboardContent{}, errors.New("invalid clipboard image encoding")
			}
			return clipboardImageContent(data, "image/png")
		}
		if platform == "darwin" {
			data, err := run(ctx, protocol.MaxSubmitInputTextBytes, "/usr/bin/pbpaste")
			return ClipboardContent{Text: string(data)}, err
		}
		data, err := run(ctx, protocol.MaxSubmitInputTextBytes, name, "-NoProfile", "-NonInteractive", "-STA", "-Command", `Add-Type -AssemblyName System.Windows.Forms; [Console]::OutputEncoding = [System.Text.Encoding]::UTF8; [Console]::Write([System.Windows.Forms.Clipboard]::GetText())`)
		return ClipboardContent{Text: string(data)}, err
	}
	if platform != "linux" {
		return ClipboardContent{}, fmt.Errorf("image paste is unsupported on %s", platform)
	}
	// Select the compositor's clipboard first; fall back if its helper is not
	// available. Never read arbitrary file paths supplied as clipboard text.
	backends := []string{"xclip", "wl-paste"}
	if wayland {
		backends = []string{"wl-paste", "xclip"}
	}
	for _, backend := range backends {
		args := []string{"-selection", "clipboard", "-t", "TARGETS", "-o"}
		if backend == "wl-paste" {
			args = []string{"--list-types"}
		}
		targets, err := run(ctx, 64<<10, backend, args...)
		if ctx.Err() != nil {
			return ClipboardContent{}, ctx.Err()
		}
		if err != nil {
			continue
		}
		available := strings.Fields(string(targets))
		for _, mime := range []string{"image/png", "image/jpeg", "image/gif"} {
			for _, target := range available {
				if target != mime {
					continue
				}
				args = []string{"-selection", "clipboard", "-t", mime, "-o"}
				if backend == "wl-paste" {
					args = []string{"--no-newline", "--type", mime}
				}
				data, err := run(ctx, protocol.MaxInputImageBytes, backend, args...)
				if err != nil {
					return ClipboardContent{}, err
				}
				return clipboardImageContent(data, mime)
			}
		}
		for _, mime := range []string{"text/plain;charset=utf-8", "UTF8_STRING", "text/plain", "STRING"} {
			for _, target := range available {
				if target != mime {
					continue
				}
				args = []string{"-selection", "clipboard", "-t", mime, "-o"}
				if backend == "wl-paste" {
					args = []string{"--no-newline", "--type", mime}
				}
				data, err := run(ctx, protocol.MaxSubmitInputTextBytes, backend, args...)
				return ClipboardContent{Text: string(data)}, err
			}
		}
		return ClipboardContent{}, errors.New("clipboard has no supported image or text (PNG/JPEG/GIF required)")
	}
	return ClipboardContent{}, errors.New("clipboard unavailable; Linux requires wl-paste (Wayland) or xclip (X11) and a desktop clipboard; SSH cannot read the local clipboard")
}

func clipboardImageContent(data []byte, mime string) (ClipboardContent, error) {
	img := protocol.InputImage{MediaType: mime, Data: data}
	if _, err := protocol.InspectInputImage(img); err != nil {
		return ClipboardContent{}, err
	}
	return ClipboardContent{Images: []protocol.InputImage{img}}, nil
}
