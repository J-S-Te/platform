import assert from 'node:assert/strict'
import test from 'node:test'

import { readFile } from 'node:fs/promises'

const source = await readFile(new URL('./navigation.js', import.meta.url), 'utf8')
const navigation = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)
const origin = 'https://platform.example.com'

test('普通 return_to 只允许当前站点', () => {
  assert.equal(navigation.resolveSameOriginRedirect('/console?tab=iam', origin), '/console?tab=iam')
  assert.equal(navigation.resolveSameOriginRedirect('https://business.example.com/home', origin), '/')
  assert.equal(navigation.resolveSameOriginRedirect('javascript:alert(1)', origin), '/')
})

test('服务端已登记登录目标允许 HTTPS 跨应用跳转', () => {
  assert.equal(
    navigation.resolveServerApprovedRedirect('https://business.example.com/home?from=platform', origin),
    'https://business.example.com/home?from=platform',
  )
  assert.equal(navigation.resolveServerApprovedRedirect('/console', origin), '/console')
  assert.equal(navigation.resolveServerApprovedRedirect('http://business.example.com/home', origin), '/')
  assert.equal(navigation.resolveServerApprovedRedirect('https://user:secret@business.example.com/home', origin), '/')
  assert.equal(navigation.resolveServerApprovedRedirect('javascript:alert(1)', origin), '/')
})
