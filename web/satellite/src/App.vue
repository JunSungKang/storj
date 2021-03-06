// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

<template>
    <div id="app">
        <router-view/>
        <!-- Area for displaying notification -->
        <NotificationArea/>
    </div>
</template>

<script lang="ts">
import { Component, Vue } from 'vue-property-decorator';

import NotificationArea from '@/components/notifications/NotificationArea.vue';

import { PartneredSatellite } from '@/types/common.ts';
import { APP_STATE_ACTIONS } from '@/utils/constants/actionNames';
import { MetaUtils } from '@/utils/meta';

@Component({
    components: {
        NotificationArea,
    },
})
export default class App extends Vue {
    /**
     * Lifecycle hook after initial render.
     * Sets up variables from meta tags from config such satellite name, etc.
     */
    public mounted(): void {
        const satelliteName = MetaUtils.getMetaContent('satellite-name');
        const partneredSatellitesJson = JSON.parse(MetaUtils.getMetaContent('partnered-satellites'));
        const isBetaSatellite = MetaUtils.getMetaContent('is-beta-satellite');
        const couponCodeUIEnabled = MetaUtils.getMetaContent('coupon-code-ui-enabled');

        if (satelliteName) {
            this.$store.dispatch(APP_STATE_ACTIONS.SET_SATELLITE_NAME, satelliteName);

            if (partneredSatellitesJson) {
                const partneredSatellites: PartneredSatellite[] = [];
                partneredSatellitesJson.forEach((partner) => {
                    const name = partner[0];
                    const address = partner[1];
                    // skip current satellite
                    if (name !== satelliteName) {
                        partneredSatellites.push(new PartneredSatellite(name, address));
                    }
                });
                this.$store.dispatch(APP_STATE_ACTIONS.SET_PARTNERED_SATELLITES, partneredSatellites);
            }
        }

        if (isBetaSatellite) {
            this.$store.dispatch(APP_STATE_ACTIONS.SET_SATELLITE_STATUS, isBetaSatellite === 'true');
        }

        if (couponCodeUIEnabled) {
            this.$store.dispatch(APP_STATE_ACTIONS.SET_COUPON_CODE_UI_STATUS, couponCodeUIEnabled === 'true');
        }

    }
}
</script>

<style lang="scss">
    html {
        overflow: hidden;
    }

    body {
        margin: 0 !important;
        height: 100vh;
        zoom: 100%;
        overflow: hidden;
    }

    img,
    a {
        -webkit-user-drag: none;
    }

    @font-face {
        font-family: 'font_regular';
        font-display: swap;
        src: url('../static/fonts/font_regular.ttf');
    }

    @font-face {
        font-family: 'font_medium';
        font-display: swap;
        src: url('../static/fonts/font_medium.ttf');
    }

    @font-face {
        font-family: 'font_bold';
        font-display: swap;
        src: url('../static/fonts/font_bold.ttf');
    }

    a {
        text-decoration: none;
        outline: none;
        cursor: pointer;
    }

    input,
    textarea {
        font-family: inherit;
        font-weight: 600;
        border: 1px solid rgba(56, 75, 101, 0.4);
        color: #354049;
        caret-color: #2683ff;
    }

    ::-webkit-scrollbar {
        width: 4px;
    }

    ::-webkit-scrollbar-track {
        box-shadow: inset 0 0 5px #fff;
    }

    ::-webkit-scrollbar-thumb {
        background: #afb7c1;
        border-radius: 6px;
        height: 5px;
    }
</style>
