/**
 * Sign-in, for instances that have accounts.
 *
 * An instance with no accounts never sees this: /api/me reports multi_user
 * false, the overlay is never built, and the console behaves exactly as the
 * single-user app it has always been. That check happens before anything else
 * loads, because every other request would otherwise 401 and the app would come
 * up empty with no explanation — which is what "multi-user support" usually
 * looks like when it is bolted on afterwards.
 */
import { $, api } from "/util.js";

/** Ask who we are. Returns the /api/me payload. */
export async function whoami() {
  try {
    return await api("/me");
  } catch {
    // An instance too old to have /api/me is single-user by definition.
    return { multi_user: false, anonymous: false, admin: true, name: "local" };
  }
}

/**
 * Block the app behind a sign-in overlay until the caller is authenticated.
 * Resolves with the signed-in identity; never resolves if the user gives up,
 * which is the intended behaviour — there is nothing to show them.
 */
export function requireSignIn(me) {
  return new Promise((resolve) => {
    const el = document.createElement("div");
    el.className = "signin";
    el.innerHTML = `
      <form class="signin-box" id="signin-form">
        <h1>Grimoire</h1>
        <p class="signin-note">Sign in to continue.</p>
        <input id="signin-name" placeholder="name" autocomplete="username" autofocus>
        <input id="signin-pass" type="password" placeholder="password" autocomplete="current-password">
        <button class="btn" id="signin-go" type="submit">Sign in</button>
        <div class="signin-error" id="signin-error"></div>
      </form>`;
    document.body.appendChild(el);
    // The readiness beacon has to fire even when nobody is signed in, or a
    // browser test (and any uptime probe) waits forever on a login screen.
    document.body.dataset.ready = "1";
    document.body.dataset.signin = "1";

    $("#signin-form").onsubmit = async (e) => {
      e.preventDefault();
      const err = $("#signin-error");
      err.textContent = "";
      try {
        const out = await api("/auth/login", {
          method: "POST",
          // api() serializes the body itself — passing a string here would
          // send a JSON-encoded JSON string, which the server reads as garbage.
          body: {
            name: $("#signin-name").value.trim(),
            password: $("#signin-pass").value,
          },
        });
        el.remove();
        delete document.body.dataset.signin;
        resolve({ ...me, anonymous: false, user: out.user, name: out.user.name,
                  admin: out.user.role === "admin" });
      } catch (ex) {
        err.textContent = ex.message || "wrong name or password";
      }
    };
  });
}

/** Show who is signed in, and offer a way out. */
export function renderIdentity(me) {
  if (!me.multi_user) return;
  const foot = $("#side-foot");
  if (!foot) return;
  const row = document.createElement("div");
  row.className = "signin-who";
  row.innerHTML = `<span title="${me.admin ? "administrator" : "member"}">${
    me.admin ? "🛡" : "👤"} ${me.name}</span>` +
    `<button class="icon" id="signout" title="sign out">⏻</button>`;
  foot.prepend(row);
  $("#signout").onclick = async () => {
    await api("/auth/logout", { method: "POST" }).catch(() => {});
    location.reload();
  };
}
