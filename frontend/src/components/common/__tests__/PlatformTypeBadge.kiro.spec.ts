import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { describe, expect, it } from 'vitest'

import PlatformTypeBadge from '../PlatformTypeBadge.vue'

const i18n = createI18n({
  legacy: false,
  locale: 'zh',
  messages: { zh: {} }
})

describe('PlatformTypeBadge Kiro platform', () => {
  it('renders Kiro instead of falling back to Gemini', () => {
    const wrapper = mount(PlatformTypeBadge, {
      props: {
        platform: 'kiro',
        type: 'oauth'
      },
      global: {
        plugins: [i18n]
      }
    })

    expect(wrapper.text()).toContain('Kiro')
    expect(wrapper.text()).toContain('OAuth')
    expect(wrapper.text()).not.toContain('Gemini')
    expect(wrapper.html()).toContain('bg-teal-100')
  })
})
