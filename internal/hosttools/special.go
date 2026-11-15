package hosttools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func readSpecial(a Args, rg string) (any, bool, error) {
	ext := strings.ToLower(filepath.Ext(a.Path))
	types := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}
	if mime := types[ext]; mime != "" {
		f, e := os.Open(a.Path)
		if e != nil {
			return nil, true, e
		}
		defer f.Close()
		b, e := io.ReadAll(io.LimitReader(f, (4<<20)+1))
		if e != nil {
			return nil, true, e
		}
		if len(b) > 4<<20 {
			return nil, true, fmt.Errorf("image exceeds 4 MiB")
		}
		return map[string]any{"path": a.Path, "sha256": digest(b), "image": map[string]any{"type": "image", "mimeType": mime, "data": base64.StdEncoding.EncodeToString(b)}}, true, nil
	}
	if ext == ".pdf" {
		binary := func(n string) string {
			if filepath.IsAbs(rg) {
				return filepath.Join(filepath.Dir(rg), n)
			}
			return n
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		info, e := exec.CommandContext(ctx, binary("pdfinfo"), a.Path).Output()
		if e != nil {
			return nil, true, fmt.Errorf("PDF reading requires pdfinfo/pdftotext in the selected location: %w", e)
		}
		total := 0
		for _, line := range strings.Split(string(info), "\n") {
			if strings.HasPrefix(line, "Pages:") {
				total, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
			}
		}
		first, last := 1, total
		if a.Pages != "" {
			parts := strings.Split(a.Pages, "-")
			if len(parts) > 2 {
				return nil, true, fmt.Errorf("pages must be N or N-M")
			}
			first, e = strconv.Atoi(parts[0])
			if e != nil {
				return nil, true, e
			}
			last = first
			if len(parts) == 2 {
				last, e = strconv.Atoi(parts[1])
				if e != nil {
					return nil, true, e
				}
			}
		} else if total > 10 {
			return nil, true, fmt.Errorf("pages is required for PDFs over 10 pages")
		}
		if first < 1 || last < first || last-first >= 20 || last > total {
			return nil, true, fmt.Errorf("invalid pages; maximum 20 pages per request")
		}
		cmd := exec.CommandContext(ctx, binary("pdftotext"), "-f", strconv.Itoa(first), "-l", strconv.Itoa(last), a.Path, "-")
		var out limitedPDF
		cmd.Stdout = &out
		var stderr cappedBuffer
		cmd.Stderr = &stderr
		if e = cmd.Run(); e != nil {
			return nil, true, fmt.Errorf("PDF text extraction: %v", e)
		}
		return map[string]any{"path": a.Path, "content": out.String(), "pages": fmt.Sprintf("%d-%d", first, last), "total_pages": total, "format": "pdf_text"}, true, nil
	}
	if a.Pages != "" {
		return nil, true, fmt.Errorf("pages applies only to PDF files")
	}
	return nil, false, nil
}

type limitedPDF struct{ bytes.Buffer }

func (w *limitedPDF) Write(p []byte) (int, error) {
	if w.Len()+len(p) > 1<<20 {
		return 0, fmt.Errorf("PDF output exceeds 1 MiB; request fewer pages")
	}
	return w.Buffer.Write(p)
}

func notebookEdit(a Args) (any, error) {
	f, e := os.Open(a.Path)
	if e != nil {
		return nil, e
	}
	b, e := readFile(f)
	f.Close()
	if e != nil {
		return nil, e
	}
	if a.SHA256 == "" || digest(b) != a.SHA256 {
		return nil, fmt.Errorf("Read this notebook before editing; the version must match")
	}
	var book map[string]any
	if e = json.Unmarshal(b, &book); e != nil {
		return nil, e
	}
	cells, ok := book["cells"].([]any)
	if !ok {
		return nil, fmt.Errorf("expected a Jupyter notebook with cells")
	}
	mode := a.EditMode
	if mode == "" {
		mode = "replace"
	}
	index := -1
	if a.CellID != "" {
		for i, c := range cells {
			if m, ok := c.(map[string]any); ok && m["id"] == a.CellID {
				index = i
				break
			}
		}
		if index < 0 {
			if n, e := strconv.Atoi(a.CellID); e == nil && n >= 0 && n < len(cells) {
				index = n
			}
		}
	}
	if mode != "insert" && index < 0 {
		return nil, fmt.Errorf("cell_id does not identify an existing cell")
	}
	if mode == "insert" && a.CellID != "" && index < 0 {
		return nil, fmt.Errorf("insert target cell_id not found")
	}
	switch mode {
	case "delete":
		cells = append(cells[:index], cells[index+1:]...)
	case "insert":
		if a.CellType == "" {
			return nil, fmt.Errorf("cell_type is required when inserting")
		}
		cell := map[string]any{"id": fmt.Sprintf("waldo-%x", time.Now().UnixNano()), "cell_type": a.CellType, "metadata": map[string]any{}, "source": strings.SplitAfter(a.NewSource, "\n")}
		if a.CellType == "code" {
			cell["outputs"] = []any{}
			cell["execution_count"] = nil
		}
		cells = append(cells, nil)
		copy(cells[index+2:], cells[index+1:])
		cells[index+1] = cell
	default:
		cell, ok := cells[index].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid cell")
		}
		cell["source"] = strings.SplitAfter(a.NewSource, "\n")
		if a.CellType != "" {
			cell["cell_type"] = a.CellType
		}
		if cell["cell_type"] == "code" {
			cell["outputs"] = []any{}
			cell["execution_count"] = nil
		} else {
			delete(cell, "outputs")
			delete(cell, "execution_count")
		}
	}
	book["cells"] = cells
	b, e = json.MarshalIndent(book, "", "  ")
	if e != nil {
		return nil, e
	}
	a.Content = string(b) + "\n"
	return modify("Write", a)
}
