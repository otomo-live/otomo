<script setup lang="ts">
import { IonApp } from '@ionic/vue'

import AppShell from '@/components/AppShell.vue'
</script>

<template>
  <IonApp>
    <!-- The shell holds the navigation and the outlet. It renders nothing but
         the outlet when no session exists, which is the sign-in screen's case. -->
    <AppShell />
  </IonApp>
</template>
