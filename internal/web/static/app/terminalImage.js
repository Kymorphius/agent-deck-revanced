// terminalImage.js -- matrix/hardened: the work head's image button.
//
// A phone cannot paste a screenshot into a tmux pane, and the host cannot see
// the phone's files. So the button uploads each picked image to the host
// (uploadSessionImage -> POST /api/sessions/{id}/image), gets back the absolute
// path it was stored under, and pastes those paths into the pane. Claude Code
// turns a pasted image path into an [Image #n] attachment, exactly as when a
// file is dragged onto a native terminal.
//
// The paste goes through terminal.paste(), like a clipboard paste (see
// terminalPaste.js), so it reaches the pane over the single guarded input path
// and arrives bracketed. No DOM or xterm import here: upload and paste are
// passed in, which keeps this module unit-testable on its own.
//
// matrix/hardened: the button sits in the work head at every screen size
// (AppShell.js). The file input stays in TerminalPanel, which owns the pane;
// it registers its picker here and the work head opens it.
import { signal } from '@preact/signals'

export const imageUploadingSignal = signal(false)

let picker = null

// registerImagePicker installs the function that opens the file chooser and
// returns the matching unregister, for a useEffect cleanup.
export function registerImagePicker(open) {
  picker = open
  return () => { if (picker === open) picker = null }
}

// openImagePicker must run inside the click handler: browsers only open a
// file chooser from a user gesture.
export function openImagePicker() {
  if (picker) picker()
}

// imagePastePath escapes a path the way a terminal escapes a dropped file, so
// a space or quote in it cannot split it into two arguments.
export function imagePastePath(path) {
  return path.replace(/([\\\s'"()&;|<>$`!*?[\]{}#~])/g, '\\$1')
}

// uploadImagesToPane uploads files one at a time and pastes every stored path
// in a single paste, followed by a space so the user can keep typing. A file
// that fails is reported in `errors` without stopping the others.
export async function uploadImagesToPane(files, { sessionId, upload, paste }) {
  const paths = []
  const errors = []
  for (const file of files) {
    try {
      paths.push(await upload(sessionId, file))
    } catch (err) {
      errors.push((file.name || 'image') + ': ' + (err?.message || 'upload failed'))
    }
  }
  if (paths.length) paste(paths.map(imagePastePath).join(' ') + ' ')
  return { pasted: paths.length, errors }
}
