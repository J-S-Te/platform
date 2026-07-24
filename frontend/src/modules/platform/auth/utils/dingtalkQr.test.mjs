import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'

const source = await readFile(new URL('./dingtalkQr.js', import.meta.url), 'utf8')
const dingtalkQr = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)

const validSDKConfig = {
  client_id: 'ding-client-001',
  redirect_uri: 'https://platform.example.com/api/v1/auth/dingtalk/callback',
  response_type: 'code',
  scope: 'openid corpid',
  state: 'QrStateToken-1234567890',
  prompt: 'consent',
}

test('只接受 dingtalk_frame 及后端下发的安全 SDK 配置', () => {
  assert.equal(dingtalkQr.isDingTalkFrameRenderMode('dingtalk_frame'), true)
  assert.equal(dingtalkQr.isDingTalkFrameRenderMode('iframe'), false)

  assert.deepEqual(dingtalkQr.normalizeDingTalkQrSDKConfig(validSDKConfig), validSDKConfig)
  assert.throws(
    () => dingtalkQr.normalizeDingTalkQrSDKConfig({ ...validSDKConfig, redirect_uri: 'http://platform.example.com/callback' }),
    /回调地址/,
  )
  assert.throws(
    () => dingtalkQr.normalizeDingTalkQrSDKConfig({ ...validSDKConfig, scope: 'corpid' }), /授权范围/)
})

test('SDK 成功回调必须匹配 state、已登记 redirect_uri 和授权码', () => {
  const callbackURL = dingtalkQr.validateDingTalkQrSDKSuccess(
    {
      redirectUrl:
        'https://platform.example.com/api/v1/auth/dingtalk/callback?authCode=code%2Fwith%2Fslash&state=QrStateToken-1234567890',
      authCode: 'code%2Fwith%2Fslash',
      state: 'QrStateToken-1234567890',
    },
    validSDKConfig,
  )

  assert.equal(
    callbackURL,
    'https://platform.example.com/api/v1/auth/dingtalk/callback?authCode=code%2Fwith%2Fslash&state=QrStateToken-1234567890',
  )
  assert.throws(
    () =>
      dingtalkQr.validateDingTalkQrSDKSuccess(
        {
          redirectUrl:
            'https://platform.example.com/api/v1/auth/dingtalk/callback?authCode=code&state=unexpected-state',
          authCode: 'code',
          state: 'unexpected-state',
        },
        validSDKConfig,
      ),
    /状态校验/,
  )
  assert.throws(
    () =>
      dingtalkQr.validateDingTalkQrSDKSuccess(
        {
          redirectUrl: 'https://untrusted.example.com/callback?authCode=code&state=QrStateToken-1234567890',
          authCode: 'code',
          state: 'QrStateToken-1234567890',
        },
        validSDKConfig,
      ),
    /回调地址校验/,
  )
})

test('固定 SDK 地址动态加载一次，安全渲染二维码且不覆盖已失效会话', async () => {
  const scripts = []
  const documentRef = {
    head: {
      appendChild(script) {
        script.parentNode = this
        scripts.push(script)
      },
    },
    querySelectorAll() {
      return scripts
    },
    createElement() {
      const listeners = new Map()
      return {
        parentNode: null,
        attributes: new Map(),
        addEventListener(name, callback) {
          const callbacks = listeners.get(name) || []
          callbacks.push(callback)
          listeners.set(name, callbacks)
        },
        dispatch(name) {
          for (const callback of listeners.get(name) || []) {
            callback()
          }
        },
        setAttribute(name, value) {
          this.attributes.set(name, value)
        },
        getAttribute(name) {
          return this.attributes.get(name) || null
        },
      }
    },
  }
  const windowRef = {}

  const firstLoad = dingtalkQr.loadDingTalkFrameLoginSDK(documentRef, windowRef)
  const secondLoad = dingtalkQr.loadDingTalkFrameLoginSDK(documentRef, windowRef)
  assert.strictEqual(firstLoad, secondLoad)
  assert.equal(scripts.length, 1)
  assert.equal(scripts[0].src, dingtalkQr.DINGTALK_FRAME_LOGIN_SDK_URL)

  let invocation
  windowRef.DTFrameLogin = (...argumentsList) => {
    invocation = argumentsList
  }
  scripts[0].dispatch('load')
  await firstLoad

  let replaceCount = 0
  const container = {
    replaceChildren() {
      replaceCount += 1
    },
  }
  documentRef.getElementById = (id) => (id === 'dingtalk-qr-frame-login' ? container : null)
  let callbackURL = ''
  const mounted = await dingtalkQr.mountDingTalkFrameLogin({
    containerID: 'dingtalk-qr-frame-login',
    sdkConfig: validSDKConfig,
    onSuccess: (value) => {
      callbackURL = value
    },
    onError: (error) => {
      throw error
    },
    documentRef,
    windowRef,
  })

  assert.equal(mounted, true)
  assert.equal(replaceCount, 1)
  assert.equal(invocation[0].id, 'dingtalk-qr-frame-login')
  assert.equal(invocation[0].width, 228)
  assert.equal(invocation[0].height, 228)
  assert.deepEqual(invocation[1], validSDKConfig)

  const previousInvocation = invocation
  const skipped = await dingtalkQr.mountDingTalkFrameLogin({
    containerID: 'dingtalk-qr-frame-login',
    sdkConfig: validSDKConfig,
    onSuccess: () => {},
    onError: () => {},
    isActive: () => false,
    documentRef,
    windowRef,
  })
  assert.equal(skipped, false)
  assert.equal(replaceCount, 1)
  assert.strictEqual(invocation, previousInvocation)

  invocation[2]({
    redirectUrl:
      'https://platform.example.com/api/v1/auth/dingtalk/callback?authCode=short-code&state=QrStateToken-1234567890',
    authCode: 'short-code',
    state: 'QrStateToken-1234567890',
  })
  assert.match(callbackURL, /^https:\/\/platform\.example\.com\/api\/v1\/auth\/dingtalk\/callback\?/)
})
