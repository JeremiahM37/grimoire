// Clip to mnemo — MV3 background service worker
async function serverUrl() {
  const { mnemoUrl } = await chrome.storage.sync.get("mnemoUrl");
  return (mnemoUrl || "http://notes.homelab.internal").replace(/\/$/, "");
}

async function clip({ text, title, url }) {
  const base = await serverUrl();
  try {
    await fetch(base + "/api/capture", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text: text || title, title, url, source: "extension" }),
    });
    chrome.action.setBadgeText({ text: "✓" });
    setTimeout(() => chrome.action.setBadgeText({ text: "" }), 1500);
  } catch (e) {
    chrome.action.setBadgeText({ text: "!" });
  }
}

chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.create({ id: "clip", title: "Clip to mnemo", contexts: ["selection", "page"] });
});

chrome.contextMenus.onClicked.addListener((info, tab) => {
  clip({ text: info.selectionText, title: tab.title, url: tab.url });
});

chrome.action.onClicked.addListener(async (tab) => {
  const [{ result }] = await chrome.scripting.executeScript({
    target: { tabId: tab.id }, func: () => window.getSelection().toString(),
  });
  clip({ text: result, title: tab.title, url: tab.url });
});
