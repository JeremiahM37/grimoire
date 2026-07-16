# mnemo browser capture

Two ways to clip the web into your vault (both hit `POST /api/capture`).

## Bookmarklet (works in any browser, nothing to install)

Make a new bookmark with this as the URL (set `MNEMO` to your server):

```js
javascript:(function(){var M='http://notes.homelab.internal';var s=window.getSelection().toString();fetch(M+'/api/capture',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({text:s||document.title,title:document.title,url:location.href,source:'bookmarklet'})}).then(function(){alert('Clipped to mnemo');});})();
```

Select text on a page, click the bookmark → it lands in your inbox and is linked
from today's daily note.

## Browser extension (Chrome/Edge/Firefox MV3)

Load `extension/` as an unpacked extension. It adds a right-click "Clip to mnemo"
context menu and a toolbar button. Set your server URL in the extension options
(defaults to `http://notes.homelab.internal`).
