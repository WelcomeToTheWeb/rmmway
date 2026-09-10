// pdf_test.go — tests for PDF report generation.
package reports

import (
	"io"
	"testing"
)

// TestGeneratePDF tests basic PDF generation with table data.
func TestGeneratePDF(t *testing.T) {
	headers := []string{"Name", "Value", "Status"}
	rows := [][]string{
		{"Device 1", "100", "OK"},
		{"Device 2", "200", "Warning"},
	}

	reader, err := GeneratePDF("Test Report", "Test Subtitle", headers, rows)
	if err != nil {
		t.Fatalf("GeneratePDF failed: %v", err)
	}

	// Read all content
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to read PDF content: %v", err)
	}

	// Verify it's a valid PDF (starts with %PDF)
	if len(data) < 5 || string(data[:5]) != "%PDF-" {
		t.Errorf("Generated PDF does not start with %%PDF header")
	}

	// Verify it's non-empty and has reasonable size
	if len(data) < 1000 {
		t.Errorf("PDF seems too small: %d bytes", len(data))
	}
}

// TestGeneratePDFEmpty tests PDF generation with no data rows.
func TestGeneratePDFEmpty(t *testing.T) {
	headers := []string{"Name", "Value"}
	rows := [][]string{}

	reader, err := GeneratePDF("Empty Report", "", headers, rows)
	if err != nil {
		t.Fatalf("GeneratePDF failed: %v", err)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to read PDF content: %v", err)
	}

	if len(data) < 5 || string(data[:5]) != "%PDF-" {
		t.Errorf("Generated PDF does not start with %%PDF header")
	}
}

// TestGeneratePDFWithSummary tests PDF generation with a summary section.
func TestGeneratePDFWithSummary(t *testing.T) {
	headers := []string{"Name", "Value"}
	rows := [][]string{
		{"Test", "Value"},
	}
	summaryItems := []string{"Item 1", "Item 2", "Item 3"}

	reader, err := GeneratePDFWithSummary("Summary Report", "Subtitle", "Summary", summaryItems, headers, rows)
	if err != nil {
		t.Fatalf("GeneratePDFWithSummary failed: %v", err)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to read PDF content: %v", err)
	}

	if len(data) < 5 || string(data[:5]) != "%PDF-" {
		t.Errorf("Generated PDF does not start with %%PDF header")
	}

	// Should be larger than basic PDF due to summary
	if len(data) < 1500 {
		t.Errorf("PDF with summary seems too small: %d bytes", len(data))
	}
}
