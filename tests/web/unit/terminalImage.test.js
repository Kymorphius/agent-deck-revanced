// matrix/hardened: the terminal strip's image button (terminalImage.js).
import { describe, it, expect, vi } from 'vitest'
import { imagePastePath, uploadImagesToPane } from '../../../internal/web/static/app/terminalImage.js'

describe('imagePastePath', () => {
  it('leaves an ordinary absolute path alone', () => {
    expect(imagePastePath('/home/claude/.agent-deck/profiles/default/uploads/s1/20261009-a1b2c3.png'))
      .toBe('/home/claude/.agent-deck/profiles/default/uploads/s1/20261009-a1b2c3.png')
  })

  it('escapes spaces and shell metacharacters like a dropped file', () => {
    expect(imagePastePath("/Users/m/App Support/it's (1).png")).toBe("/Users/m/App\\ Support/it\\'s\\ \\(1\\).png")
  })
})

describe('uploadImagesToPane', () => {
  it('uploads every file and pastes all paths once, with a trailing space', async () => {
    const upload = vi.fn(async (_id, f) => '/up/' + f.name)
    const paste = vi.fn()
    const res = await uploadImagesToPane([{ name: 'a.png' }, { name: 'b.jpg' }], { sessionId: 's1', upload, paste })
    expect(upload).toHaveBeenCalledTimes(2)
    expect(upload.mock.calls[0][0]).toBe('s1')
    expect(paste).toHaveBeenCalledTimes(1)
    expect(paste).toHaveBeenCalledWith('/up/a.png /up/b.jpg ')
    expect(res).toEqual({ pasted: 2, errors: [] })
  })

  it('reports a failed file and still pastes the rest', async () => {
    const upload = vi.fn(async (_id, f) => {
      if (f.name === 'bad.txt') throw new Error('only PNG, JPEG, GIF or WebP images')
      return '/up/' + f.name
    })
    const paste = vi.fn()
    const res = await uploadImagesToPane([{ name: 'bad.txt' }, { name: 'ok.png' }], { sessionId: 's1', upload, paste })
    expect(paste).toHaveBeenCalledWith('/up/ok.png ')
    expect(res.pasted).toBe(1)
    expect(res.errors).toEqual(['bad.txt: only PNG, JPEG, GIF or WebP images'])
  })

  it('pastes nothing when every upload fails', async () => {
    const paste = vi.fn()
    const res = await uploadImagesToPane([{ name: 'x.png' }], {
      sessionId: 's1',
      upload: async () => { throw new Error('too many requests') },
      paste,
    })
    expect(paste).not.toHaveBeenCalled()
    expect(res).toEqual({ pasted: 0, errors: ['x.png: too many requests'] })
  })
})
