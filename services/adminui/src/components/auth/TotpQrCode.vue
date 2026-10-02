<script setup lang="ts">
import QRCode from 'qrcode'
import { onMounted, ref, watch } from 'vue'

/**
 * The enrollment QR, rendered as a data URL.
 *
 * A data URL rather than inline SVG: it needs no `v-html`, and the colours come
 * from the design tokens at render time so the code follows the theme. The
 * caller supplies the `otpauth://` URI; this is deliberately just the image.
 */

const props = defineProps<{ otpauthUrl: string }>()

const dataUrl = ref('')

async function render(url: string): Promise<void> {
  let dark = ''
  let light = ''
  try {
    const style = getComputedStyle(document.documentElement)
    dark = style.getPropertyValue('--ds-text').trim()
    light = style.getPropertyValue('--ds-surface').trim()
  } catch {
    // No computed styles (a test environment): `qrcode`'s own high-contrast
    // default is the fallback, and no colour is named here.
  }
  dataUrl.value = await QRCode.toDataURL(url, {
    errorCorrectionLevel: 'M',
    margin: 1,
    width: 240,
    ...(dark !== '' && light !== '' ? { color: { dark, light } } : {}),
  })
}

onMounted(() => render(props.otpauthUrl))
watch(
  () => props.otpauthUrl,
  (url) => void render(url),
)
</script>

<template>
  <img v-if="dataUrl !== ''" class="totp-qr" :src="dataUrl" alt="TOTP enrollment QR code" />
</template>

<style scoped>
.totp-qr {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-md);
  padding: var(--ds-space-2);
}
</style>
