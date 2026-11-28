/**
 * Triggers a browser file download from an in-memory string.
 * Intentionally keeps the anchor element detached from document.body so click events
 * do not bubble into Grafana's delegated anchor/router interceptors.
 */
export function downloadBlob(filename: string, content: string, mimeType: string): void {
  const blob = new Blob([content], { type: mimeType });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}
