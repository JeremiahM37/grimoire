import { test } from 'node:test'
import assert from 'node:assert/strict'
import { memoryTopic, toGrimoirePath, toVaultPath } from '../src/paths.ts'

test('same folder: paths pass through', () => {
  assert.equal(toVaultPath('memory/ops.md', ''), 'memory/ops.md')
  assert.equal(toGrimoirePath('memory/ops.md', ''), 'memory/ops.md')
  assert.equal(memoryTopic('memory/ops.md', ''), 'ops')
})

test('grimoire serving a subfolder of the vault', () => {
  assert.equal(toVaultPath('memory/ops.md', 'Work/'), 'Work/memory/ops.md')
  assert.equal(toVaultPath('/memory/ops.md', '/Work'), 'Work/memory/ops.md')
  assert.equal(toGrimoirePath('Work/memory/ops.md', 'Work'), 'memory/ops.md')
  assert.equal(memoryTopic('Work/memory/team/ops.md', 'Work'), 'team/ops')
})

test('files outside the served folder are not Grimoire notes', () => {
  assert.equal(toGrimoirePath('Personal/diary.md', 'Work'), null)
  assert.equal(toGrimoirePath('Workshop/x.md', 'Work'), null, 'a prefix is not a folder')
  assert.equal(memoryTopic('Personal/memory/x.md', 'Work'), null)
  assert.equal(memoryTopic('runbooks/ops.md', ''), null)
})
