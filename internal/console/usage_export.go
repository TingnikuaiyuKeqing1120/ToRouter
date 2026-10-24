package console

import (
	"io"
	"net/http"
	"os"
	"time"

	"torouter/internal/store"
)

func usageExportHandler(s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := filter(r)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		// Do not expose a successful CSV until the complete snapshot has been read
		// and written. Temporary files contain only the explicitly exported columns.
		file, err := os.CreateTemp("", "torouter-usage-*.csv")
		if err != nil {
			fail(w, http.StatusInternalServerError, "cannot prepare usage export")
			return
		}
		defer func() { file.Close(); os.Remove(file.Name()) }()
		if err := s.ExportUsageCSV(r.Context(), f, file); err != nil {
			fail(w, http.StatusInternalServerError, "cannot export usage")
			return
		}
		info, err := file.Stat()
		if err != nil {
			fail(w, http.StatusInternalServerError, "cannot prepare usage export")
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="usage.csv"`)
		http.ServeContent(w, r, "usage.csv", time.Time{}, io.NewSectionReader(file, 0, info.Size()))
	}
}
