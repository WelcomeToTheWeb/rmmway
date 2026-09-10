// pdf.go — PDF generation for report types (gap #8b).
//
// Uses gofppdf to generate professional-looking PDF reports for all 5
// report types: Fleet Status, Device Report, Patch Compliance, License
// Compliance, and Uptime/SLA.
package reports

import (
	"bytes"
	"fmt"
	"io"
	"time"

	"github.com/jung-kurt/gofpdf"
)

// PDFReport is the interface for generating PDF reports.
type PDFReport interface {
	Generate() (io.Reader, error)
}

// basePDFReport provides common PDF generation infrastructure.
type basePDFReport struct {
	title       string
	subtitle    string
	company     string
	generatedAt time.Time
}

func newBasePDFReport(title, subtitle, company string) *basePDFReport {
	return &basePDFReport{
		title:       title,
		subtitle:    subtitle,
		company:     company,
		generatedAt: time.Now().UTC(),
	}
}

// generateHeader creates the report header with title, company, and timestamp.
func (b *basePDFReport) generateHeader(pdf *gofpdf.Fpdf) {
	pdf.SetTextColor(51, 102, 153)
	pdf.SetFont("Helvetica", "B", 24)
	pdf.CellFormat(0, 10, b.title, "", 1, "L", false, 0, "")

	if b.subtitle != "" {
		pdf.SetTextColor(100, 100, 100)
		pdf.SetFont("Helvetica", "", 14)
		pdf.CellFormat(0, 8, b.subtitle, "", 1, "L", false, 0, "")
	}

	pdf.SetTextColor(150, 150, 150)
	pdf.SetFont("Helvetica", "I", 10)
	pdf.CellFormat(0, 6, fmt.Sprintf("Generated: %s", b.generatedAt.Format("2006-01-02 15:04:05 MST")), "", 1, "L", false, 0, "")

	if b.company != "" {
		pdf.CellFormat(0, 6, fmt.Sprintf("Company: %s", b.company), "", 1, "L", false, 0, "")
	}

	pdf.SetDrawColor(200, 200, 200)
	_, w := pdf.GetPageSize()
	pdf.Line(10, pdf.GetY(), w-10, pdf.GetY())
	pdf.Ln(5)
}

// writeTable writes a table to the PDF.
func (b *basePDFReport) writeTable(pdf *gofpdf.Fpdf, headers []string, rows [][]string) {
	_, pageW := pdf.GetPageSize()
	// Header row
	pdf.SetFillColor(51, 102, 153)
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFont("Helvetica", "B", 10)

	xPos := 10.0
	yPos := pdf.GetY()

	// Calculate column widths (equal distribution)
	colWidth := (pageW - 20) / float64(len(headers))

	for _, header := range headers {
		pdf.SetXY(xPos, yPos)
		pdf.CellFormat(colWidth, 8, header, "1", 0, "L", true, 0, "")
		xPos += colWidth
	}
	pdf.Ln(8)

	// Data rows
	pdf.SetTextColor(0, 0, 0)
	pdf.SetFont("Helvetica", "", 9)

	for rowIdx, row := range rows {
		xPos = 10.0
		if rowIdx%2 == 1 {
			pdf.SetFillColor(255, 255, 255)
		} else {
			pdf.SetFillColor(245, 245, 245)
		}

		for _, cell := range row {
			pdf.SetXY(xPos, pdf.GetY())
			pdf.CellFormat(colWidth, 6, cell, "1", 0, "L", true, 0, "")
			xPos += colWidth
		}
		pdf.Ln(6)
	}
}

// writeSummaryBox creates a summary box with key metrics.
func (b *basePDFReport) writeSummaryBox(pdf *gofpdf.Fpdf, title string, items []string) {
	pdf.SetFillColor(240, 248, 255)
	pdf.SetTextColor(51, 102, 153)
	pdf.SetFont("Helvetica", "B", 12)
	pdf.CellFormat(0, 8, title, "", 1, "L", true, 0, "")
	pdf.Ln(3)

	pdf.SetTextColor(0, 0, 0)
	pdf.SetFont("Helvetica", "", 10)
	for _, item := range items {
		pdf.CellFormat(0, 6, "  • "+item, "", 1, "L", false, 0, "")
	}
	pdf.Ln(5)
}

// writeFooter adds the footer with page numbers.
func (b *basePDFReport) writeFooter(pdf *gofpdf.Fpdf) {
	pdf.SetTextColor(150, 150, 150)
	pdf.SetFont("Helvetica", "I", 8)
	pdf.CellFormat(0, 5, fmt.Sprintf("Page %d", pdf.PageNo()), "", 0, "C", false, 0, "")
}

// GeneratePDF generates a basic PDF report with the given title and content rows.
func GeneratePDF(title, subtitle string, headers []string, rows [][]string) (io.Reader, error) {
	pdf := gofpdf.New("P", "mm", "A4", "")
	pdf.SetTitle(title, false)
	pdf.AddPage()

	b := newBasePDFReport(title, subtitle, "")
	b.generateHeader(pdf)

	if len(rows) == 0 {
		pdf.SetFont("Helvetica", "", 10)
		pdf.SetTextColor(100, 100, 100)
		pdf.CellFormat(0, 10, "No data available for this report period.", "", 1, "C", false, 0, "")
	} else {
		b.writeTable(pdf, headers, rows)
	}

	pdf.SetTextColor(0, 0, 0)
	pdf.SetFont("Helvetica", "", 9)
	pdf.Ln(5)
	pdf.CellFormat(0, 5, fmt.Sprintf("Report generated on %s", b.generatedAt.Format("2006-01-02 15:04:05 MST")), "", 1, "L", false, 0, "")

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("output PDF: %w", err)
	}
	return &buf, nil
}

// GeneratePDFWithSummary generates a PDF with a summary section and table.
func GeneratePDFWithSummary(title, subtitle, summaryTitle string, summaryItems []string, headers []string, rows [][]string) (io.Reader, error) {
	pdf := gofpdf.New("P", "mm", "A4", "")
	pdf.SetTitle(title, false)
	pdf.AddPage()

	b := newBasePDFReport(title, subtitle, "")
	b.generateHeader(pdf)

	if len(summaryItems) > 0 {
		b.writeSummaryBox(pdf, summaryTitle, summaryItems)
	}

	if len(rows) == 0 {
		pdf.SetFont("Helvetica", "", 10)
		pdf.SetTextColor(100, 100, 100)
		pdf.CellFormat(0, 10, "No detailed data available for this report period.", "", 1, "C", false, 0, "")
	} else {
		pdf.SetFont("Helvetica", "B", 12)
		pdf.SetTextColor(51, 102, 153)
		pdf.CellFormat(0, 8, "Detailed Report", "", 1, "L", false, 0, "")
		pdf.Ln(3)
		b.writeTable(pdf, headers, rows)
	}

	pdf.SetTextColor(0, 0, 0)
	pdf.SetFont("Helvetica", "", 9)
	pdf.Ln(5)
	pdf.CellFormat(0, 5, fmt.Sprintf("Report generated on %s", b.generatedAt.Format("2006-01-02 15:04:05 MST")), "", 1, "L", false, 0, "")

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("output PDF: %w", err)
	}
	return &buf, nil
}
