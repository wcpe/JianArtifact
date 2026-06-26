// 多格式依赖坐标纯函数测试（FR-93）：
// GAV 反解与后端 Gav::from_path 对齐；8 种坐标片段内容；HTML View / 下载 URL。

import { describe, it, expect } from 'vitest';
import { parseMavenGav, buildCoordinateSnippets, htmlViewUrl, downloadUrl } from './coordinates';

describe('parseMavenGav', () => {
  it('四段以上路径反解 GAV（倒数第三段=artifactId、倒二=version、前缀=group）', () => {
    expect(parseMavenGav('com/example/foo/lib/1.2.3/lib-1.2.3.jar')).toEqual({
      groupId: 'com.example.foo',
      artifactId: 'lib',
      version: '1.2.3',
    });
  });

  it('单段 group 也可反解', () => {
    expect(parseMavenGav('com/example/lib/1.0/lib-1.0.jar')).toEqual({
      groupId: 'com.example',
      artifactId: 'lib',
      version: '1.0',
    });
  });

  it('段数不足返回 null（目录级 metadata / 裸文件名）', () => {
    expect(parseMavenGav('com/foo/maven-metadata.xml')).toBeNull();
    expect(parseMavenGav('lib-1.0.jar')).toBeNull();
  });

  it('忽略前导斜杠与空段', () => {
    expect(parseMavenGav('/com/example/lib/1.0/lib-1.0.jar')).toEqual({
      groupId: 'com.example',
      artifactId: 'lib',
      version: '1.0',
    });
  });
});

describe('buildCoordinateSnippets（Maven 主构件全套）', () => {
  const snippets = buildCoordinateSnippets('maven', 'com/example/lib/1.0/lib-1.0.jar');
  const byLabel = (label: string) => snippets.find((s) => s.label === label)!;

  it('产出 8 种坐标格式', () => {
    expect(snippets.map((s) => s.label)).toEqual([
      'Apache Maven',
      'Gradle Groovy DSL',
      'Gradle Kotlin DSL',
      'Scala SBT',
      'Apache Ivy',
      'Groovy Grape',
      'Leiningen',
      'PURL',
    ]);
  });

  it('Apache Maven 为 <dependency> 块', () => {
    const c = byLabel('Apache Maven').content;
    expect(c).toContain('<groupId>com.example</groupId>');
    expect(c).toContain('<artifactId>lib</artifactId>');
    expect(c).toContain('<version>1.0</version>');
  });

  it('Gradle Groovy DSL 为单引号短坐标', () => {
    expect(byLabel('Gradle Groovy DSL').content).toBe("implementation 'com.example:lib:1.0'");
  });

  it('Gradle Kotlin DSL 为双引号函数调用', () => {
    expect(byLabel('Gradle Kotlin DSL').content).toBe('implementation("com.example:lib:1.0")');
  });

  it('Scala SBT 为 %% 风格坐标', () => {
    expect(byLabel('Scala SBT').content).toBe(
      'libraryDependencies += "com.example" % "lib" % "1.0"',
    );
  });

  it('Apache Ivy 为 dependency 标签', () => {
    expect(byLabel('Apache Ivy').content).toBe(
      '<dependency org="com.example" name="lib" rev="1.0" />',
    );
  });

  it('Groovy Grape 为 @Grab 注解', () => {
    expect(byLabel('Groovy Grape').content).toBe(
      "@Grab(group='com.example', module='lib', version='1.0')",
    );
  });

  it('Leiningen 为向量坐标', () => {
    expect(byLabel('Leiningen').content).toBe('[com.example/lib "1.0"]');
  });

  it('PURL 为 pkg:maven 坐标', () => {
    expect(byLabel('PURL').content).toBe('pkg:maven/com.example/lib@1.0');
  });
});

describe('buildCoordinateSnippets（非 Maven / 无法反解）', () => {
  it('npm / docker / raw 不产出 Maven 全套坐标（空数组）', () => {
    expect(buildCoordinateSnippets('npm', 'lodash/-/lodash-4.17.21.tgz')).toEqual([]);
    expect(buildCoordinateSnippets('docker', 'library/nginx/latest')).toEqual([]);
    expect(buildCoordinateSnippets('raw', 'dir/a.txt')).toEqual([]);
  });

  it('Maven 但路径无法反解 GAV（目录级 metadata）→ 空数组', () => {
    expect(buildCoordinateSnippets('maven', 'com/foo/maven-metadata.xml')).toEqual([]);
  });
});

describe('htmlViewUrl', () => {
  it('指向制品所在目录的索引（尾斜杠）', () => {
    expect(htmlViewUrl('files', 'dir/a.txt')).toBe('/files/dir/');
  });

  it('多级目录取父目录', () => {
    expect(htmlViewUrl('m', 'com/example/lib/1.0/lib-1.0.jar')).toBe('/m/com/example/lib/1.0/');
  });

  it('根目录文件回退到仓库根索引', () => {
    expect(htmlViewUrl('files', 'top.txt')).toBe('/files/');
  });

  it('逐段编码仓库名与目录段', () => {
    expect(htmlViewUrl('my repo', 'a b/c.txt')).toBe('/my%20repo/a%20b/');
  });
});

describe('downloadUrl', () => {
  it('指向制品原始下载路径（无尾斜杠）', () => {
    expect(downloadUrl('files', 'dir/a.txt')).toBe('/files/dir/a.txt');
  });

  it('逐段编码保留分隔斜杠', () => {
    expect(downloadUrl('my repo', 'a b/c d.txt')).toBe('/my%20repo/a%20b/c%20d.txt');
  });
});
