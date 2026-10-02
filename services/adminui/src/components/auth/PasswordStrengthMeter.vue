<script setup lang="ts">
import { computed } from 'vue'

import { passwordStrength } from '@/auth/passwordStrength'

/**
 * The four-bar strength hint, shared by onboarding and the account page.
 *
 * It is a hint and not the policy: the server validates the password and its 400
 * is what the form shows. `passwordStrength` owns the score; this only paints it.
 */

const props = defineProps<{
  password: string
  /** Used to penalise a password built from the address. Optional. */
  email?: string
}>()

const strength = computed(() => passwordStrength(props.password, props.email ?? ''))
</script>

<template>
  <div class="strength">
    <div class="strength__row" data-testid="password-strength">
      <div class="strength__meter" :data-score="strength.score">
        <span
          v-for="bar in 4"
          :key="bar"
          class="strength__bar"
          :class="{ 'strength__bar--on': bar <= strength.score }"
        />
      </div>
      <span class="strength__label" data-testid="password-strength-label">{{
        strength.label
      }}</span>
    </div>
    <ul v-if="strength.hints.length > 0" class="strength__hints">
      <li v-for="hint in strength.hints" :key="hint">{{ hint }}</li>
    </ul>
  </div>
</template>

<style scoped>
.strength {
  margin-top: var(--ds-space-2);
}

.strength__row {
  align-items: center;
  display: flex;
  gap: var(--ds-space-2);
}

.strength__meter {
  display: flex;
  gap: var(--ds-space-1);
}

.strength__bar {
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  display: block;
  height: 6px;
  width: 36px;
}

.strength__meter[data-score='1'] .strength__bar--on,
.strength__meter[data-score='2'] .strength__bar--on {
  background: var(--ds-warn);
}

.strength__meter[data-score='3'] .strength__bar--on,
.strength__meter[data-score='4'] .strength__bar--on {
  background: var(--ds-ok);
}

.strength__label {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
}

.strength__hints {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-2) 0 0;
  padding-left: var(--ds-space-5);
}
</style>
