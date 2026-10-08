// The installed app's title/status bar takes its colour from <meta name="theme-color">.
// index.html ships a light and a dark tag (by system scheme); this keeps them in step with
// the in-app theme toggle, which can disagree with the system scheme.
const LIGHT = '#faf7f0', DARK = '#1a1814';
export function syncThemeColor() {
  const forced = document.documentElement.dataset.theme;
  const dark = forced ? forced === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
  document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]').forEach(meta => {
    const media = meta.getAttribute('media') || '';
    meta.content = forced ? (dark ? DARK : LIGHT) : media.includes('dark') ? DARK : LIGHT;
  });
  // With no forced theme the media-qualified tags decide; otherwise both say the same thing.
}
export function watchThemeColor() {
  syncThemeColor();
  new MutationObserver(syncThemeColor).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', syncThemeColor);
}
