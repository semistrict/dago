export const meta = {
  name: 'browser-release-sweep',
  description: 'Exercise the workflow operations panel',
  phases: [{title: 'Inspect', detail: 'Deterministic browser fixture'}],
}

phase('Inspect')
log('Fixture complete')
return {ready: true}
