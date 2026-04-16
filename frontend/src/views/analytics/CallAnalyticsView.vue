<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle
} from '@/components/ui/card'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Skeleton } from '@/components/ui/skeleton'
import { Button } from '@/components/ui/button'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue
} from '@/components/ui/select'
import { Badge } from '@/components/ui/badge'
import { toast } from 'vue-sonner'
import {
  PhoneCall,
  PhoneMissed,
  PhoneIncoming,
  PhoneOutgoing,
  Clock,
  Activity,
  BarChart3
} from 'lucide-vue-next'
import { PageHeader, ErrorState, DateRangePicker } from '@/components/shared'
import { Line, Bar, Doughnut } from '@/lib/charts'
import { useDateRange } from '@/composables/useDateRange'
import { callAnalyticsService, type CallAnalyticsResponse } from '@/services/api'
import { getErrorMessage } from '@/lib/api-utils'

// Call Analytics Dashboard.
//
// Renders KPI cards, a daily trend line chart, hourly distribution bar
// chart, status + channel donut charts, and tables of top IVR flows +
// top agents. All data comes from a single /api/analytics/calls call
// so filter state (date range, channel, direction) applies atomically
// across the whole page.

const { t } = useI18n()

const analytics = ref<CallAnalyticsResponse | null>(null)
const isLoading = ref(true)
const error = ref<string | null>(null)

const selectedChannel = ref<'' | 'whatsapp' | 'telnyx_pstn'>('')
const selectedDirection = ref<'' | 'incoming' | 'outgoing'>('')

const {
  selectedRange,
  customDateRange,
  isDatePickerOpen,
  dateRange,
  formatDateRangeDisplay,
  applyCustomRange: applyCustomRangeBase
} = useDateRange({ storageKey: 'call_analytics' })

async function fetchAnalytics() {
  isLoading.value = true
  error.value = null
  try {
    const response = await callAnalyticsService.get({
      start_date: dateRange.value.from,
      end_date: dateRange.value.to,
      channel: selectedChannel.value || undefined,
      direction: selectedDirection.value || undefined
    })
    const data = (response.data as any).data || response.data
    analytics.value = data as CallAnalyticsResponse
  } catch (e) {
    error.value = getErrorMessage(e, t('callAnalytics.fetchError'))
    toast.error(error.value)
  } finally {
    isLoading.value = false
  }
}

function applyCustomRange() {
  applyCustomRangeBase()
  fetchAnalytics()
}

watch([selectedRange, selectedChannel, selectedDirection], () => {
  if (selectedRange.value !== 'custom') fetchAnalytics()
})

onMounted(() => fetchAnalytics())

// --- Derived chart data ---------------------------------------------------

const dailyTrendChart = computed(() => {
  const trend = analytics.value?.daily_trend || []
  return {
    labels: trend.map(p => p.date),
    datasets: [
      {
        label: t('callAnalytics.series.total'),
        data: trend.map(p => p.total),
        borderColor: '#6366f1',
        backgroundColor: 'rgba(99, 102, 241, 0.08)',
        tension: 0.3,
        fill: true
      },
      {
        label: t('callAnalytics.series.answered'),
        data: trend.map(p => p.answered),
        borderColor: '#10b981',
        backgroundColor: 'rgba(16, 185, 129, 0.08)',
        tension: 0.3
      },
      {
        label: t('callAnalytics.series.missed'),
        data: trend.map(p => p.missed),
        borderColor: '#ef4444',
        backgroundColor: 'rgba(239, 68, 68, 0.08)',
        tension: 0.3
      }
    ]
  }
})

const hourlyChart = computed(() => {
  const hours = analytics.value?.hourly_distribution || []
  return {
    labels: hours.map(h => String(h.hour).padStart(2, '0') + 'h'),
    datasets: [
      {
        label: t('callAnalytics.series.calls'),
        data: hours.map(h => h.total),
        backgroundColor: 'rgba(99, 102, 241, 0.7)',
        borderColor: '#6366f1',
        borderWidth: 1
      }
    ]
  }
})

const statusChart = computed(() => {
  const status = analytics.value?.status_breakdown || []
  const palette: Record<string, string> = {
    completed: '#10b981',
    answered: '#22d3ee',
    accepted: '#3b82f6',
    ringing: '#a78bfa',
    missed: '#ef4444',
    rejected: '#f59e0b',
    failed: '#6b7280',
    initiating: '#9ca3af',
    transferring: '#f472b6'
  }
  return {
    labels: status.map(s => statusLabel(s.status)),
    datasets: [
      {
        data: status.map(s => s.count),
        backgroundColor: status.map(s => palette[s.status] || '#94a3b8'),
        borderWidth: 0
      }
    ]
  }
})

const channelChart = computed(() => {
  const ch = analytics.value?.channel_breakdown || []
  const palette: Record<string, string> = {
    whatsapp: '#25d366',
    telnyx_pstn: '#6366f1'
  }
  return {
    labels: ch.map(c => channelLabel(c.channel)),
    datasets: [
      {
        data: ch.map(c => c.count),
        backgroundColor: ch.map(c => palette[c.channel] || '#94a3b8'),
        borderWidth: 0
      }
    ]
  }
})

// --- Helpers --------------------------------------------------------------

function formatDuration(secs: number): string {
  if (!secs || secs <= 0) return '0s'
  const s = Math.round(secs)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  const rem = s - m * 60
  if (m < 60) return `${m}m ${rem}s`
  const h = Math.floor(m / 60)
  const mRem = m - h * 60
  return `${h}h ${mRem}m`
}

function formatPercent(v: number): string {
  return `${(v * 100).toFixed(1)}%`
}

function statusLabel(s: string): string {
  const key = `callAnalytics.statuses.${s}`
  const translated = t(key)
  // If the key falls through to itself, default to the raw string.
  return translated === key ? s : translated
}

function channelLabel(c: string): string {
  return c === 'telnyx_pstn' ? t('callAnalytics.channels.telnyx') : t('callAnalytics.channels.whatsapp')
}

const chartOptions = computed(() => ({
  responsive: true,
  maintainAspectRatio: false,
  plugins: {
    legend: {
      position: 'bottom' as const,
      labels: { color: '#94a3b8', font: { size: 11 } }
    }
  },
  scales: {
    x: {
      ticks: { color: '#94a3b8' },
      grid: { color: 'rgba(148, 163, 184, 0.1)' }
    },
    y: {
      ticks: { color: '#94a3b8' },
      grid: { color: 'rgba(148, 163, 184, 0.1)' },
      beginAtZero: true
    }
  }
}))

const donutOptions = computed(() => ({
  responsive: true,
  maintainAspectRatio: false,
  plugins: {
    legend: {
      position: 'bottom' as const,
      labels: { color: '#94a3b8', font: { size: 11 }, padding: 10 }
    }
  }
}))
</script>

<template>
  <div class="flex flex-col h-full bg-[#0a0a0b] light:bg-gray-50">
    <PageHeader
      :title="$t('callAnalytics.title')"
      :subtitle="$t('callAnalytics.subtitle')"
      :icon="BarChart3"
      icon-gradient="bg-gradient-to-br from-indigo-500 to-sky-600 shadow-indigo-500/20"
    >
      <template #actions>
        <div class="flex items-center gap-2 flex-wrap">
          <!-- Channel filter -->
          <Select v-model="selectedChannel">
            <SelectTrigger class="h-9 w-40 text-sm">
              <SelectValue :placeholder="$t('callAnalytics.filters.allChannels')" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="">{{ $t('callAnalytics.filters.allChannels') }}</SelectItem>
              <SelectItem value="whatsapp">{{ $t('callAnalytics.channels.whatsapp') }}</SelectItem>
              <SelectItem value="telnyx_pstn">{{ $t('callAnalytics.channels.telnyx') }}</SelectItem>
            </SelectContent>
          </Select>
          <!-- Direction filter -->
          <Select v-model="selectedDirection">
            <SelectTrigger class="h-9 w-40 text-sm">
              <SelectValue :placeholder="$t('callAnalytics.filters.allDirections')" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="">{{ $t('callAnalytics.filters.allDirections') }}</SelectItem>
              <SelectItem value="incoming">{{ $t('callAnalytics.filters.incoming') }}</SelectItem>
              <SelectItem value="outgoing">{{ $t('callAnalytics.filters.outgoing') }}</SelectItem>
            </SelectContent>
          </Select>
          <!-- Date range -->
          <DateRangePicker
            v-model:selected-range="selectedRange"
            v-model:custom-date-range="customDateRange"
            v-model:is-date-picker-open="isDatePickerOpen"
            :display-text="formatDateRangeDisplay"
            @apply="applyCustomRange"
          />
        </div>
      </template>
    </PageHeader>

    <ErrorState
      v-if="error && !isLoading"
      :title="$t('callAnalytics.fetchErrorTitle')"
      :description="error"
      :retry-label="$t('common.retry')"
      class="flex-1"
      @retry="fetchAnalytics"
    />

    <ScrollArea v-else class="flex-1">
      <div class="p-6 space-y-6 max-w-7xl mx-auto">
        <!-- KPI Cards -->
        <div class="grid grid-cols-2 md:grid-cols-4 gap-4">
          <Card>
            <CardContent class="pt-6">
              <div class="flex items-center justify-between">
                <div class="space-y-1">
                  <p class="text-xs text-muted-foreground uppercase tracking-wide">
                    {{ $t('callAnalytics.kpi.totalCalls') }}
                  </p>
                  <Skeleton v-if="isLoading" class="h-8 w-16" />
                  <p v-else class="text-2xl font-bold">{{ analytics?.summary.total_calls || 0 }}</p>
                </div>
                <PhoneCall class="h-8 w-8 text-indigo-500 opacity-60" />
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardContent class="pt-6">
              <div class="flex items-center justify-between">
                <div class="space-y-1">
                  <p class="text-xs text-muted-foreground uppercase tracking-wide">
                    {{ $t('callAnalytics.kpi.answered') }}
                  </p>
                  <Skeleton v-if="isLoading" class="h-8 w-20" />
                  <p v-else class="text-2xl font-bold">
                    {{ analytics?.summary.answered_calls || 0 }}
                    <span class="text-sm font-normal text-muted-foreground ml-1">
                      ({{ formatPercent(analytics?.summary.answered_rate || 0) }})
                    </span>
                  </p>
                </div>
                <PhoneIncoming class="h-8 w-8 text-emerald-500 opacity-60" />
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardContent class="pt-6">
              <div class="flex items-center justify-between">
                <div class="space-y-1">
                  <p class="text-xs text-muted-foreground uppercase tracking-wide">
                    {{ $t('callAnalytics.kpi.missed') }}
                  </p>
                  <Skeleton v-if="isLoading" class="h-8 w-20" />
                  <p v-else class="text-2xl font-bold">
                    {{ analytics?.summary.missed_calls || 0 }}
                    <span class="text-sm font-normal text-muted-foreground ml-1">
                      ({{ formatPercent(analytics?.summary.missed_rate || 0) }})
                    </span>
                  </p>
                </div>
                <PhoneMissed class="h-8 w-8 text-red-500 opacity-60" />
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardContent class="pt-6">
              <div class="flex items-center justify-between">
                <div class="space-y-1">
                  <p class="text-xs text-muted-foreground uppercase tracking-wide">
                    {{ $t('callAnalytics.kpi.avgDuration') }}
                  </p>
                  <Skeleton v-if="isLoading" class="h-8 w-16" />
                  <p v-else class="text-2xl font-bold">
                    {{ formatDuration(analytics?.summary.avg_duration_secs || 0) }}
                  </p>
                </div>
                <Clock class="h-8 w-8 text-sky-500 opacity-60" />
              </div>
            </CardContent>
          </Card>
        </div>

        <!-- Secondary KPI strip -->
        <div class="grid grid-cols-2 md:grid-cols-3 gap-4">
          <Card>
            <CardContent class="pt-6">
              <p class="text-xs text-muted-foreground uppercase tracking-wide">
                {{ $t('callAnalytics.kpi.incomingOutgoing') }}
              </p>
              <div class="flex items-center gap-3 mt-2">
                <div class="flex items-center gap-1">
                  <PhoneIncoming class="h-4 w-4 text-emerald-500" />
                  <span class="text-lg font-semibold">{{ analytics?.summary.incoming_calls || 0 }}</span>
                </div>
                <span class="text-muted-foreground">·</span>
                <div class="flex items-center gap-1">
                  <PhoneOutgoing class="h-4 w-4 text-indigo-500" />
                  <span class="text-lg font-semibold">{{ analytics?.summary.outgoing_calls || 0 }}</span>
                </div>
              </div>
            </CardContent>
          </Card>
          <Card>
            <CardContent class="pt-6">
              <p class="text-xs text-muted-foreground uppercase tracking-wide">
                {{ $t('callAnalytics.kpi.totalTalkTime') }}
              </p>
              <p class="text-lg font-semibold mt-2">
                {{ formatDuration(analytics?.summary.total_duration_secs || 0) }}
              </p>
            </CardContent>
          </Card>
          <Card>
            <CardContent class="pt-6">
              <p class="text-xs text-muted-foreground uppercase tracking-wide">
                {{ $t('callAnalytics.kpi.range') }}
              </p>
              <p class="text-sm mt-2">
                {{ analytics?.range.start_date || '—' }} → {{ analytics?.range.end_date || '—' }}
              </p>
            </CardContent>
          </Card>
        </div>

        <!-- Daily trend chart -->
        <Card>
          <CardHeader>
            <CardTitle>{{ $t('callAnalytics.dailyTrend') }}</CardTitle>
            <CardDescription>{{ $t('callAnalytics.dailyTrendDesc') }}</CardDescription>
          </CardHeader>
          <CardContent>
            <Skeleton v-if="isLoading" class="h-[280px] w-full" />
            <div v-else class="h-[280px]">
              <Line :data="dailyTrendChart" :options="chartOptions" />
            </div>
          </CardContent>
        </Card>

        <!-- 2-up row: hourly + status -->
        <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
          <Card>
            <CardHeader>
              <CardTitle>{{ $t('callAnalytics.hourlyDistribution') }}</CardTitle>
              <CardDescription>{{ $t('callAnalytics.hourlyDistributionDesc') }}</CardDescription>
            </CardHeader>
            <CardContent>
              <Skeleton v-if="isLoading" class="h-[240px] w-full" />
              <div v-else class="h-[240px]">
                <Bar :data="hourlyChart" :options="chartOptions" />
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>{{ $t('callAnalytics.statusBreakdown') }}</CardTitle>
              <CardDescription>{{ $t('callAnalytics.statusBreakdownDesc') }}</CardDescription>
            </CardHeader>
            <CardContent>
              <Skeleton v-if="isLoading" class="h-[240px] w-full" />
              <div v-else class="h-[240px]">
                <Doughnut :data="statusChart" :options="donutOptions" />
              </div>
            </CardContent>
          </Card>
        </div>

        <!-- 2-up row: channel + top IVR flows -->
        <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
          <Card>
            <CardHeader>
              <CardTitle>{{ $t('callAnalytics.channelBreakdown') }}</CardTitle>
              <CardDescription>{{ $t('callAnalytics.channelBreakdownDesc') }}</CardDescription>
            </CardHeader>
            <CardContent>
              <Skeleton v-if="isLoading" class="h-[240px] w-full" />
              <div v-else class="h-[240px]">
                <Doughnut :data="channelChart" :options="donutOptions" />
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>{{ $t('callAnalytics.topIVRFlows') }}</CardTitle>
              <CardDescription>{{ $t('callAnalytics.topIVRFlowsDesc') }}</CardDescription>
            </CardHeader>
            <CardContent>
              <Skeleton v-if="isLoading" class="h-[240px] w-full" />
              <div v-else-if="!analytics?.top_ivr_flows.length" class="text-sm text-muted-foreground py-4 text-center">
                {{ $t('callAnalytics.noData') }}
              </div>
              <div v-else class="space-y-2">
                <div
                  v-for="flow in analytics.top_ivr_flows"
                  :key="flow.flow_id"
                  class="flex items-center justify-between text-sm"
                >
                  <span class="truncate">{{ flow.flow_name }}</span>
                  <Badge variant="secondary">{{ flow.count }}</Badge>
                </div>
              </div>
            </CardContent>
          </Card>
        </div>

        <!-- Top agents -->
        <Card>
          <CardHeader>
            <CardTitle>{{ $t('callAnalytics.topAgents') }}</CardTitle>
            <CardDescription>{{ $t('callAnalytics.topAgentsDesc') }}</CardDescription>
          </CardHeader>
          <CardContent>
            <Skeleton v-if="isLoading" class="h-[160px] w-full" />
            <div v-else-if="!analytics?.top_agents.length" class="text-sm text-muted-foreground py-4 text-center">
              {{ $t('callAnalytics.noData') }}
            </div>
            <table v-else class="w-full text-sm">
              <thead>
                <tr class="border-b">
                  <th class="text-left font-medium py-2">{{ $t('callAnalytics.agents.name') }}</th>
                  <th class="text-right font-medium py-2">{{ $t('callAnalytics.agents.handled') }}</th>
                  <th class="text-right font-medium py-2">{{ $t('callAnalytics.agents.avgDuration') }}</th>
                  <th class="text-right font-medium py-2">{{ $t('callAnalytics.agents.totalDuration') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="agent in analytics.top_agents"
                  :key="agent.agent_id"
                  class="border-b last:border-0"
                >
                  <td class="py-2">{{ agent.agent_name }}</td>
                  <td class="py-2 text-right">{{ agent.handled }}</td>
                  <td class="py-2 text-right">{{ formatDuration(agent.avg_duration_secs) }}</td>
                  <td class="py-2 text-right">{{ formatDuration(agent.total_duration_secs) }}</td>
                </tr>
              </tbody>
            </table>
          </CardContent>
        </Card>
      </div>
    </ScrollArea>
  </div>
</template>
