/* ── helmstudio: its runtime, its components ──────────────────────────────
 *
 * iris studio draws its own takes rail, its own preview and its own terminal,
 * and keeps drawing them. These are the two things it cannot draw: the gallery
 * every studio shares, which holds what iris studio recorded beside what the
 * others did, and the render's log as helmstudio streams it.
 *
 * Everything here comes through the /helm/ proxy the server mounts, so the
 * page holds no token and nothing of helmstudio's is copied into this
 * repository. If /helm/ cannot answer, the imports fail, the Gallery button
 * stays hidden, and the rest of the page is untouched.
 *
 * The components are given no URL and build none. They read a client, which is
 * window.helm here — helm-ui's own rule, and why one page sets it once.
 */

"use strict";

(() => {
  const HELM_SDK = "/helm/sdk/v1";

  /** An element with attributes, text and children, like app.js builds them. */
  function el(tag, attrs, ...kids) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(attrs || {})) {
      if (key === "text") node.textContent = value;
      else if (value !== false && value != null) node.setAttribute(key, value === true ? "" : value);
    }
    node.append(...kids.filter(Boolean));
    return node;
  }

  /**
   * Put a component inside a container that is already in the document, and
   * return it.
   *
   * Not createElement: a custom element made that way does not always upgrade,
   * and comes back as HTMLUnknownElement that never renders. The parser
   * upgrades it, so the element is written as markup into a container that is
   * in the document, which is when a custom element is upgraded.
   *
   * The markup is a fixed tag with fixed attributes and carries no data, so
   * this is not the innerHTML that untrusted text would need escaping for.
   */
  function mountComponent(container, markup) {
    container.insertAdjacentHTML("beforeend", markup);
    return container.lastElementChild;
  }

  /**
   * The gallery: iris studio's own takes, as helmstudio recorded them.
   *
   * One <helm-gallery> for the page, not one per opening: it holds an event
   * stream open so a take that lands appears without a reload, and a browser
   * gives a page only so many connections to one host.
   */
  function galleryDialog() {
    const title = el("h2", { class: "helmstudio-title", text: "Gallery" });
    const status = el("span", { class: "helmstudio-status", role: "status" });
    const close = el("button", { class: "ghost-btn sm", type: "button", text: "Close" });
    const head = el("div", { class: "helmstudio-head" },
      title, status, el("span", { class: "helmstudio-spacer" }), close);
    const d = el("dialog", { class: "helmstudio-dialog" }, head);
    document.body.append(d);
    const gallery = mountComponent(d, '<helm-gallery scope="self" kind="image"></helm-gallery>');

    let choose = null;
    const finish = (item) => {
      if (!choose) return;
      const resolve = choose;
      choose = null;
      gallery.removeAttribute("picker");
      title.textContent = "Gallery";
      status.textContent = "";
      resolve(item);
    };

    close.addEventListener("click", () => d.close());
    // The component says what happened and decides nothing: browsing emits
    // select, picking emits pick. What either means is this studio's business.
    gallery.addEventListener("select", (event) => {
      const item = event.detail && event.detail.item;
      if (!choose) status.textContent = item ? `${item.id} selected` : "";
    });
    gallery.addEventListener("pick", (event) => {
      finish(event.detail && event.detail.item);
      d.close();
    });
    // Closing a picker answers nothing rather than hanging the caller.
    d.addEventListener("close", () => finish(null));

    return {
      browse() {
        d.showModal();
      },
      /** Open as a picker; resolves to the chosen item, or null if dismissed. */
      pick(purpose) {
        finish(null);
        title.textContent = purpose;
        status.textContent = "Pick an image, then Use this.";
        gallery.setAttribute("picker", "");
        d.showModal();
        return new Promise((resolve) => {
          choose = resolve;
        });
      },
    };
  }

  /**
   * Copy a gallery image into this session's inputs/, so it can be attached as
   * a reference.
   *
   * A reference is a file iris reads from disk when it renders, so an asset
   * that lives in helmstudio's library has to become one here first. The bytes
   * come through the /helm/ proxy, which adds the token the page never holds,
   * and go back out through the same upload the drop zone uses — so the
   * server, the session and the inputs list all behave as they always do, and
   * the "inputs" event it emits is what redraws the rail.
   *
   * Any studio's image will do. That is the point of a shared gallery.
   */
  async function useAsReference(item) {
    const asset = item.asset || {};
    const response = await fetch(`/helm/api/v1/assets/${encodeURIComponent(item.asset_id)}`);
    if (!response.ok) throw new Error(`helmstudio answered ${response.status}`);
    const bytes = await response.blob();
    // Named after the take, not after the asset: a file called by its id tells
    // nobody which picture it is. The extension follows the bytes.
    const stem = (item.title || "reference").replace(/[^\w.-]+/g, "-").replace(/^-+|-+$/g, "") || "reference";
    const ext = (asset.mime || bytes.type || "image/png").split("/").pop().replace("jpeg", "jpg");
    await fetch("/api/upload", {
      method: "POST",
      headers: { "X-Filename": `${stem}.${ext}` },
      body: bytes,
    }).then((r) => {
      if (!r.ok) throw new Error(`the upload was refused (${r.status})`);
    });
  }

  /**
   * The render log: helm-terminal streaming the job helmstudio keeps.
   *
   * It sits in its own tab beside Output rather than replacing it. iris
   * studio's terminal is one pane for three things — the render's output,
   * shell commands and interactive iris — and helm-terminal streams exactly
   * one job, so replacing it would trade two of those away. What it adds is
   * what iris studio's cannot do: ANSI colour, and reconnection from the last
   * line it saw, so a dropped stream resumes and says what it missed instead
   * of losing it in silence.
   */
  function mountRenderLog() {
    const pane = document.getElementById("renderLogPane");
    const tabs = document.getElementById("terminalTabs");
    const log = document.getElementById("terminalLog");
    const inputRow = document.getElementById("terminalInputRow");
    const clear = document.getElementById("termClearBtn");
    if (!pane || !tabs || !log) return;

    const term = mountComponent(pane, "<helm-terminal follow></helm-terminal>");
    term.client = window.helm;
    tabs.hidden = false;

    // Output's own controls belong to Output: the input line runs commands
    // against iris studio's terminal and Clear empties its buffer, and
    // helm-terminal brings its own of both.
    const show = (which) => {
      const render = which === "render";
      pane.hidden = !render;
      log.hidden = render;
      if (inputRow) inputRow.hidden = render;
      if (clear) clear.hidden = render;
      for (const tab of tabs.querySelectorAll("button")) {
        tab.classList.toggle("active", tab.dataset.tab === which);
      }
    };
    tabs.addEventListener("click", (event) => {
      const tab = event.target.closest("button[data-tab]");
      if (tab) show(tab.dataset.tab);
    });
    show("output");

    // app.js calls this on every job update. Until this runs there is no job
    // log to follow, so the property is absent and app.js's optional call does
    // nothing — which is the page as it behaves with no helmstudio at all.
    window.showHelmRenderLog = (job) => {
      const id = job && job.helm_job;
      // No job means nothing to stream: an older render from before this ran,
      // or a studio started without helmstudio. Clearing is what tells the
      // component to stop rather than keep showing a finished render's log.
      if (!id) {
        term.removeAttribute("job");
        return;
      }
      if (term.getAttribute("job") !== id) term.setAttribute("job", id);
    };
  }

  /**
   * Connect to helmstudio, if this studio is running under it.
   *
   * Failure is not an error: the server mounts /helm/ but the daemon behind it
   * can be down or still starting, and the page must be the same page either
   * way. So this gives up quietly and says nothing.
   */
  async function connectHelmstudio() {
    let connect;
    try {
      ({ connect } = await import(`${HELM_SDK}/helm-runtime.js`));
      await import(`${HELM_SDK}/helm-ui.js`);
    } catch {
      return; // nothing behind the proxy: no components, no Gallery button
    }
    // helm-ui components read window.helm when they are given no client of
    // their own, which is how one page serves every component it mounts.
    window.helm = connect();

    const gallery = galleryDialog();
    const button = document.getElementById("galleryButton");
    if (button) {
      button.hidden = false;
      button.addEventListener("click", () => gallery.browse());
    }

    const fromGallery = document.getElementById("refFromGallery");
    if (fromGallery) {
      fromGallery.hidden = false;
      fromGallery.addEventListener("click", async () => {
        const item = await gallery.pick("Use an image as a reference");
        if (!item) return;
        try {
          await useAsReference(item);
        } catch (err) {
          window.alert(`That image could not be used as a reference: ${err.message}`);
        }
      });
    }

    mountRenderLog();
  }

  connectHelmstudio();
})();
