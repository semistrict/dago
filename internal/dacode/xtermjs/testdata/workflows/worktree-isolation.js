export const meta = {
  name: 'browser-worktree-isolation',
  description: 'Verify isolated workflow workers through the terminal host',
  phases: [{title: 'Worktree', detail: 'Create, commit, and verify cleanup'}],
}

const WORKTREE = {
  type: 'object',
  additionalProperties: false,
  required: ['path', 'branch', 'commit'],
  properties: {
    path: {type: 'string'},
    branch: {type: 'string'},
    commit: {type: 'string'},
  },
}

phase('Worktree')
const isolated = await agent('WORKTREE_ISOLATION_CREATE: create and commit the requested fixture change, then report the checkout path, branch, and commit.', {
  label: 'create-isolated-change',
  phase: 'Worktree',
  isolation: 'worktree',
  schema: WORKTREE,
})
if (!isolated) throw new Error('isolated worker failed')

const verification = await agent('WORKTREE_ISOLATION_VERIFY: verify the committed branch remains and its linked checkout was removed.\n' + JSON.stringify(isolated), {
  label: 'verify-isolated-change',
  phase: 'Worktree',
})
if (verification !== 'WORKTREE_E2E_OK') throw new Error('worktree verification failed')
log(verification)
return {isolated, verification}
