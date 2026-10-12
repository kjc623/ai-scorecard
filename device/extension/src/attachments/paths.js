/**
 * attachments/paths.js — where on disk the files a tab uploaded came from. Runs in the service
 * worker.
 *
 * A page never learns the path of a file the user chose (an input reports `C:\fakepath\…`), but
 * when the page uploads the file in a multipart form the browser names it in the body it hands the
 * body lane: the file's part is `{file: path}` in place of its bytes. A chat page uploads a file as
 * soon as it is chosen and sends the prompt that names it later, so each path is held for its tab
 * until a submission from that tab names a file of the same name, or for `HOLD_MS`.
 *
 * A file uploaded any other way (a raw PUT, a form the page submits itself, bytes the page built)
 * leaves no path, and its descriptor goes without one.
 */

/** How long a path waits for the submission that names its file. */
export const HOLD_MS = 30 * 60_000;
/** The paths held across every tab; past it the oldest is let go. */
export const MAX_HELD = 64;
/** protocol.MaxAttachmentPathRunes: a longer path would make the record invalid. */
export const MAX_PATH_CHARS = 4096;

export function createUploadPaths({ clock = () => Date.now(), holdMs = HOLD_MS, maxHeld = MAX_HELD } = {}) {
  /** @type {{tabId: number, path: string, at: number}[]} oldest first */
  let held = [];

  function expire() {
    const cut = clock() - holdMs;
    held = held.filter((h) => h.at >= cut);
  }

  /** Hold the path of each file the request uploads from its tab. */
  function note(detail) {
    const raw = detail && detail.requestBody && detail.requestBody.raw;
    if (!Array.isArray(raw) || !Number.isInteger(detail.tabId) || detail.tabId < 0) return;
    expire();
    for (const part of raw) {
      const path = part && part.file;
      if (typeof path !== 'string' || path === '' || [...path].length > MAX_PATH_CHARS) continue;
      held.push({ tabId: detail.tabId, path, at: clock() });
    }
    if (held.length > maxHeld) held = held.slice(-maxHeld);
  }

  /**
   * The descriptors, each with the path of the newest file of its name the tab uploaded. A path is
   * given to one descriptor only.
   */
  function place(tabId, descriptors) {
    expire();
    return descriptors.map((d) => {
      if (!d || typeof d.name !== 'string' || d.path) return d;
      for (let i = held.length - 1; i >= 0; i--) {
        if (held[i].tabId === tabId && namesFile(held[i].path, d.name)) {
          const [{ path }] = held.splice(i, 1);
          return { ...d, path };
        }
      }
      return d;
    });
  }

  return { note, place };
}

/** Whether `path` is the path of a file called `name`, on either separator. */
function namesFile(path, name) {
  return path.endsWith(`/${name}`) || path.endsWith(`\\${name}`);
}
